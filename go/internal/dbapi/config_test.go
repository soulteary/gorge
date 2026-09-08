package dbapi

import (
	"os"
	"path/filepath"
	"testing"
)

// The config tests are ported from the standalone service's
// internal/cluster/config_test.go, rewritten against this package's
// Config/BuildCluster split. The standalone service parsed a RawConfig; here
// the single-node path comes from the GORGE_DB_* env (or their legacy
// fallbacks) and the cluster path from a Phorge local.json, so the tests cover
// both entry points into the same ClusterConfig.

// clearDBEnv unsets every variable LoadFromEnv reads so a test starts from the
// documented defaults regardless of the caller's environment.
func clearDBEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"GORGE_DB_MYSQL_HOST", "MYSQL_HOST",
		"GORGE_DB_MYSQL_PORT", "MYSQL_PORT",
		"GORGE_DB_MYSQL_USER", "MYSQL_USER",
		"GORGE_DB_MYSQL_PASS", "MYSQL_PASS",
		"GORGE_DB_NAMESPACE", "STORAGE_NAMESPACE",
		"GORGE_DB_CONFIG_FILE", "PHORGE_CONFIG",
	} {
		t.Setenv(key, "")
	}
}

func TestLoadFromEnvDefaults(t *testing.T) {
	clearDBEnv(t)

	cfg := LoadFromEnv()
	if cfg.MySQLHost != DefaultMySQLHost {
		t.Errorf("host = %q, want %q", cfg.MySQLHost, DefaultMySQLHost)
	}
	if cfg.MySQLPort != DefaultMySQLPort {
		t.Errorf("port = %d, want %d", cfg.MySQLPort, DefaultMySQLPort)
	}
	if cfg.MySQLUser != DefaultMySQLUser {
		t.Errorf("user = %q, want %q", cfg.MySQLUser, DefaultMySQLUser)
	}
	if cfg.Namespace != DefaultNamespace {
		t.Errorf("namespace = %q, want %q", cfg.Namespace, DefaultNamespace)
	}
}

// TestLoadFromEnvPrefersNewNames pins the newest-name-first fallback: a
// GORGE_DB_ variable always wins over the legacy unprefixed one, which is how
// a pre-monorepo deployment keeps working while a migrated one takes over.
func TestLoadFromEnvPrefersNewNames(t *testing.T) {
	clearDBEnv(t)
	t.Setenv("MYSQL_HOST", "legacy-host")
	t.Setenv("GORGE_DB_MYSQL_HOST", "new-host")
	t.Setenv("STORAGE_NAMESPACE", "legacy-ns")
	t.Setenv("GORGE_DB_NAMESPACE", "new-ns")

	cfg := LoadFromEnv()
	if cfg.MySQLHost != "new-host" {
		t.Errorf("GORGE_DB_MYSQL_HOST should win, got %q", cfg.MySQLHost)
	}
	if cfg.Namespace != "new-ns" {
		t.Errorf("GORGE_DB_NAMESPACE should win, got %q", cfg.Namespace)
	}
}

// TestLoadFromEnvLegacyFallback pins the other half: with only the legacy names
// set, they are used.
func TestLoadFromEnvLegacyFallback(t *testing.T) {
	clearDBEnv(t)
	t.Setenv("MYSQL_HOST", "legacy-host")
	t.Setenv("MYSQL_PORT", "3307")
	t.Setenv("MYSQL_USER", "legacy-user")
	t.Setenv("MYSQL_PASS", "legacy-pass")
	t.Setenv("STORAGE_NAMESPACE", "legacy-ns")

	cfg := LoadFromEnv()
	if cfg.MySQLHost != "legacy-host" {
		t.Errorf("host = %q", cfg.MySQLHost)
	}
	if cfg.MySQLPort != 3307 {
		t.Errorf("port = %d", cfg.MySQLPort)
	}
	if cfg.MySQLUser != "legacy-user" {
		t.Errorf("user = %q", cfg.MySQLUser)
	}
	if cfg.MySQLPass != "legacy-pass" {
		t.Errorf("pass = %q", cfg.MySQLPass)
	}
	if cfg.Namespace != "legacy-ns" {
		t.Errorf("namespace = %q", cfg.Namespace)
	}
}

// TestLoadFromEnvInvalidPortFallsBack: a non-numeric port is ignored and the
// default is kept rather than crashing the boot.
func TestLoadFromEnvInvalidPortFallsBack(t *testing.T) {
	clearDBEnv(t)
	t.Setenv("GORGE_DB_MYSQL_PORT", "notanumber")

	cfg := LoadFromEnv()
	if cfg.MySQLPort != DefaultMySQLPort {
		t.Errorf("invalid port should fall back to %d, got %d", DefaultMySQLPort, cfg.MySQLPort)
	}
}

// TestBuildClusterSingleNode: with no config file, BuildCluster builds one
// master that is individual and the default partition — the stock single-node
// Phorge deployment.
func TestBuildClusterSingleNode(t *testing.T) {
	cfg := &Config{MySQLHost: "db1", MySQLPort: 3307, MySQLUser: "app", Namespace: "phorge"}
	cc, err := cfg.BuildCluster()
	if err != nil {
		t.Fatal(err)
	}
	if len(cc.Refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(cc.Refs))
	}
	ref := cc.Refs[0]
	if !ref.IsMaster || !ref.IsIndividual || !ref.IsDefaultPartition {
		t.Error("single node should be master, individual, default partition")
	}
	if ref.Host != "db1" || ref.Port != 3307 {
		t.Errorf("host=%s port=%d", ref.Host, ref.Port)
	}
	if len(cc.Masters()) != 1 {
		t.Errorf("expected 1 master, got %d", len(cc.Masters()))
	}
}

func TestBuildClusterSingleNodeDefaultNamespace(t *testing.T) {
	cfg := &Config{MySQLHost: "localhost"}
	cc, err := cfg.BuildCluster()
	if err != nil {
		t.Fatal(err)
	}
	if cc.Namespace != DefaultNamespace {
		t.Errorf("expected default namespace %q, got %q", DefaultNamespace, cc.Namespace)
	}
}

// writeConfigFile writes a Phorge-style local.json and returns its path.
func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildClusterFromFile(t *testing.T) {
	path := writeConfigFile(t, `{
		"mysql.host": "filehost",
		"mysql.port": 3307,
		"mysql.user": "fileuser",
		"mysql.pass": "filepass",
		"storage.default-namespace": "testns"
	}`)
	cfg := &Config{ConfigFile: path}
	cc, err := cfg.BuildCluster()
	if err != nil {
		t.Fatalf("BuildCluster: %v", err)
	}
	if cc.Namespace != "testns" {
		t.Errorf("namespace = %q, want testns", cc.Namespace)
	}
	if len(cc.Refs) != 1 || cc.Refs[0].Host != "filehost" {
		t.Errorf("expected host filehost, got %v", cc.Refs)
	}
	if cc.MySQLPass != "filepass" {
		t.Errorf("pass = %q, want filepass", cc.MySQLPass)
	}
}

func TestBuildClusterFromFileMissing(t *testing.T) {
	cfg := &Config{
		ConfigFile: "/nonexistent/path/config.json",
		MySQLHost:  "fallback", MySQLPort: 3307, MySQLUser: "app", Namespace: "fallback_ns",
	}
	cc, err := cfg.BuildCluster()
	if err != nil {
		t.Fatal(err)
	}
	if len(cc.Refs) != 1 || cc.Refs[0].Host != "fallback" || cc.Refs[0].Port != 3307 {
		t.Fatalf("missing config did not fall back to scalar node: %+v", cc.Refs)
	}
}

func TestBuildClusterFromFileInvalidJSON(t *testing.T) {
	path := writeConfigFile(t, "{invalid")
	cfg := &Config{ConfigFile: path, MySQLHost: "fallback", Namespace: "fallback_ns"}
	cc, err := cfg.BuildCluster()
	if err != nil {
		t.Fatal(err)
	}
	if len(cc.Refs) != 1 || cc.Refs[0].Host != "fallback" || cc.Namespace != "fallback_ns" {
		t.Fatalf("invalid config did not fall back to scalar node: refs=%+v namespace=%q", cc.Refs, cc.Namespace)
	}
}

func TestBuildClusterAcceptsRolesObjectAndScalarPartition(t *testing.T) {
	path := writeConfigFile(t, `{
		"mysql.user": "root",
		"cluster.databases": [
			{"host": "master1", "roles": {"master": true}, "partition": "default"},
			{"host": "replica1", "roles": {"replica": true}}
		]
	}`)
	cc, err := (&Config{ConfigFile: path}).BuildCluster()
	if err != nil {
		t.Fatalf("BuildCluster rejected a compatible cluster shape: %v", err)
	}
	if len(cc.Masters()) != 1 || cc.Masters()[0].Host != "master1" {
		t.Fatalf("roles.master was not parsed: %+v", cc.Refs)
	}
	if !cc.Masters()[0].IsDefaultPartition {
		t.Fatal("scalar default partition was not parsed")
	}
	if len(cc.replicas) != 1 || cc.replicas[0].Host != "replica1" {
		t.Fatalf("roles.replica was not parsed: %+v", cc.Refs)
	}
}

func TestBuildClusterMultiNode(t *testing.T) {
	path := writeConfigFile(t, `{
		"mysql.user": "root",
		"storage.default-namespace": "phorge",
		"cluster.databases": [
			{"host": "master1", "port": 3306, "role": "master", "partition": ["default"]},
			{"host": "master2", "port": 3306, "role": "master", "partition": ["maniphest"]},
			{"host": "replica1", "port": 3306, "role": "replica"}
		]
	}`)
	cc, err := (&Config{ConfigFile: path}).BuildCluster()
	if err != nil {
		t.Fatal(err)
	}
	if len(cc.Refs) != 3 {
		t.Fatalf("expected 3 refs, got %d", len(cc.Refs))
	}

	if m := cc.GetMasterForApplication("maniphest"); m == nil || m.Host != "master2" {
		t.Errorf("expected master2 for maniphest, got %v", m)
	}
	if md := cc.GetMasterForApplication("config"); md == nil || md.Host != "master1" {
		t.Errorf("expected master1 (default) for config, got %v", md)
	}
	if r := cc.GetReplicaForApplication("config"); r == nil || r.Host != "replica1" {
		t.Errorf("expected replica1, got %v", r)
	}
}

func TestClusterNodePasswordOverridesGlobalPassword(t *testing.T) {
	globalSecret := "global-secret"
	nodeSecret := "node-secret"
	cc := buildClusterFromRaw(rawClusterFile{
		MysqlPass: &globalSecret,
		ClusterDBs: []nodeSpec{
			{Host: "custom", Role: "master", Pass: &nodeSecret},
			{Host: "fallback", Role: "replica"},
		},
	}, &Config{})

	if got := cc.Refs[0].Password; got != "node-secret" {
		t.Fatalf("custom node password = %q, want node-secret", got)
	}
	if got := cc.Refs[1].Password; got != "global-secret" {
		t.Fatalf("fallback node password = %q, want global-secret", got)
	}
	if got := cc.Refs[0].toContract(); got.User == "" && got.RefKey == "" {
		t.Fatal("unexpected empty contract projection")
	}

	ref := cc.Refs[0]
	if got := NewDiffService(cc, cc.MySQLPass).buildDSN(ref).Password; got != "node-secret" {
		t.Errorf("schema DSN password = %q", got)
	}
	if got := NewSetupService(cc, cc.MySQLPass).buildDSN(ref).Password; got != "node-secret" {
		t.Errorf("setup DSN password = %q", got)
	}
	if got := NewMigrationService(cc, cc.MySQLPass).buildDSN(ref).Password; got != "node-secret" {
		t.Errorf("migration DSN password = %q", got)
	}
	if got := NewHealthService(cc).buildDSN(ref, cc.MySQLPass).Password; got != "node-secret" {
		t.Errorf("health DSN password = %q", got)
	}
	if got := NewRouter(cc, cc.MySQLPass).buildDSN(ref, "config").Password; got != "node-secret" {
		t.Errorf("router DSN password = %q", got)
	}
}

func TestClusterScalarExplicitEmptyPasswordOverridesEnvironment(t *testing.T) {
	path := writeConfigFile(t, `{
		"mysql.host": "passwordless",
		"mysql.pass": ""
	}`)
	cc, err := (&Config{ConfigFile: path, MySQLPass: "environment-secret"}).BuildCluster()
	if err != nil {
		t.Fatal(err)
	}
	if cc.MySQLPass != "" {
		t.Fatalf("cluster password = %q, want explicit empty password", cc.MySQLPass)
	}
	if got := cc.Refs[0].passwordOr("wrong-fallback"); got != "" {
		t.Fatalf("single-node password = %q, want explicit empty password", got)
	}
}

func TestClusterNodeExplicitEmptyPasswordOverridesGlobalPassword(t *testing.T) {
	path := writeConfigFile(t, `{
		"mysql.pass": "global-secret",
		"cluster.databases": [
			{"host": "passwordless", "role": "master", "pass": ""}
		]
	}`)
	cc, err := (&Config{ConfigFile: path}).BuildCluster()
	if err != nil {
		t.Fatal(err)
	}
	ref := cc.Refs[0]
	if !ref.PasswordSet {
		t.Fatal("explicit empty node password was treated as omitted")
	}
	for name, got := range map[string]string{
		"schema":    NewDiffService(cc, cc.MySQLPass).buildDSN(ref).Password,
		"setup":     NewSetupService(cc, cc.MySQLPass).buildDSN(ref).Password,
		"migration": NewMigrationService(cc, cc.MySQLPass).buildDSN(ref).Password,
		"health":    NewHealthService(cc).buildDSN(ref, cc.MySQLPass).Password,
		"router":    NewRouter(cc, cc.MySQLPass).buildDSN(ref, "config").Password,
	} {
		if got != "" {
			t.Errorf("%s DSN password = %q, want explicit empty password", name, got)
		}
	}
}

func TestBuildClusterNodeInheritsDefaults(t *testing.T) {
	path := writeConfigFile(t, `{
		"mysql.host": "shared-host",
		"mysql.port": 3307,
		"mysql.user": "admin",
		"cluster.databases": [ {"role": "master"} ]
	}`)
	cc, err := (&Config{ConfigFile: path}).BuildCluster()
	if err != nil {
		t.Fatal(err)
	}
	ref := cc.Refs[0]
	if ref.Host != "shared-host" || ref.Port != 3307 || ref.User != "admin" {
		t.Errorf("node should inherit defaults, got %+v", ref)
	}
}

func TestBuildClusterNodeOverridesDefaults(t *testing.T) {
	path := writeConfigFile(t, `{
		"mysql.host": "shared-host",
		"mysql.user": "admin",
		"cluster.databases": [ {"host": "custom-host", "port": 3308, "user": "custom", "role": "master"} ]
	}`)
	cc, err := (&Config{ConfigFile: path}).BuildCluster()
	if err != nil {
		t.Fatal(err)
	}
	ref := cc.Refs[0]
	if ref.Host != "custom-host" || ref.Port != 3308 || ref.User != "custom" {
		t.Errorf("node should override defaults, got %+v", ref)
	}
}

func TestBuildClusterDisabledNode(t *testing.T) {
	path := writeConfigFile(t, `{
		"cluster.databases": [
			{"host": "m1", "role": "master", "disabled": true},
			{"host": "m2", "role": "master"}
		]
	}`)
	cc, err := (&Config{ConfigFile: path}).BuildCluster()
	if err != nil {
		t.Fatal(err)
	}
	if len(cc.Refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(cc.Refs))
	}
	if !cc.Refs[0].Disabled {
		t.Error("first ref should be disabled")
	}
	if cc.Refs[1].Disabled {
		t.Error("second ref should not be disabled")
	}
}

func TestBuildClusterPartitionMapping(t *testing.T) {
	path := writeConfigFile(t, `{
		"cluster.databases": [ {"host": "m1", "role": "master", "partition": ["default", "files"]} ]
	}`)
	cc, err := (&Config{ConfigFile: path}).BuildCluster()
	if err != nil {
		t.Fatal(err)
	}
	ref := cc.Refs[0]
	if !ref.IsDefaultPartition {
		t.Error("ref should be default partition when partition includes 'default'")
	}
	if !ref.ApplicationMap["files"] {
		t.Error("ref should have 'files' in application map")
	}
	if ref.ApplicationMap["default"] {
		t.Error("'default' should NOT be in application map, it sets IsDefaultPartition")
	}
}

func TestBuildClusterMasterWithoutPartitionIsDefault(t *testing.T) {
	path := writeConfigFile(t, `{
		"cluster.databases": [ {"host": "m1", "role": "master"} ]
	}`)
	cc, err := (&Config{ConfigFile: path}).BuildCluster()
	if err != nil {
		t.Fatal(err)
	}
	if !cc.Refs[0].IsDefaultPartition {
		t.Error("master without partition should be default")
	}
}

func TestBuildClusterReplicaWithoutPartitionNotDefault(t *testing.T) {
	path := writeConfigFile(t, `{
		"cluster.databases": [
			{"host": "m1", "role": "master"},
			{"host": "r1", "role": "replica"}
		]
	}`)
	cc, err := (&Config{ConfigFile: path}).BuildCluster()
	if err != nil {
		t.Fatal(err)
	}
	if cc.Refs[1].IsDefaultPartition {
		t.Error("replica without explicit partition should not be default")
	}
}

func TestDatabaseName(t *testing.T) {
	cc := &ClusterConfig{Namespace: "phorge"}
	cases := map[string]string{
		"config":    "phorge_config",
		"meta_data": "phorge_meta_data",
		"user":      "phorge_user",
	}
	for app, want := range cases {
		if got := cc.DatabaseName(app); got != want {
			t.Errorf("DatabaseName(%q) = %q, want %q", app, got, want)
		}
	}
}

func TestRefKey(t *testing.T) {
	if k := (&DatabaseRef{Host: "db1", Port: 3306}).RefKey(); k != "db1:3306" {
		t.Errorf("RefKey with port = %q, want db1:3306", k)
	}
	if k := (&DatabaseRef{Host: "db2"}).RefKey(); k != "db2" {
		t.Errorf("RefKey without port = %q, want db2", k)
	}
}

func TestGetMasterForApplicationDisabledSkipped(t *testing.T) {
	cc := &ClusterConfig{masters: []*DatabaseRef{
		{Host: "m1", IsDefaultPartition: true, Disabled: true},
		{Host: "m2", IsDefaultPartition: true},
	}}
	if m := cc.GetMasterForApplication("anything"); m == nil || m.Host != "m2" {
		t.Errorf("expected m2 (first non-disabled default), got %v", m)
	}
}

func TestGetMasterForApplicationPreferSpecific(t *testing.T) {
	cc := &ClusterConfig{masters: []*DatabaseRef{
		{Host: "m1", IsDefaultPartition: true},
		{Host: "m2", ApplicationMap: map[string]bool{"files": true}},
	}}
	if m := cc.GetMasterForApplication("files"); m == nil || m.Host != "m2" {
		t.Errorf("expected m2 for specific app, got %v", m)
	}
	if m := cc.GetMasterForApplication("config"); m == nil || m.Host != "m1" {
		t.Errorf("expected m1 (default) for unmatched app, got %v", m)
	}
}

func TestServesApplicationExcludesDefaultWhenSpecificExists(t *testing.T) {
	defaultMaster := &DatabaseRef{Host: "default", IsMaster: true, IsDefaultPartition: true}
	metadataMaster := &DatabaseRef{
		Host: "metadata", IsMaster: true,
		ApplicationMap: map[string]bool{"meta_data": true},
	}
	cc := &ClusterConfig{masters: []*DatabaseRef{defaultMaster, metadataMaster}}
	if !cc.ServesApplication(metadataMaster, "meta_data") {
		t.Fatal("explicit metadata master should own meta_data")
	}
	if cc.ServesApplication(defaultMaster, "meta_data") {
		t.Fatal("default master must not also own meta_data when an explicit partition exists")
	}
}

func TestServesApplicationMatchesReplicaFallbackRouting(t *testing.T) {
	filesMaster := &DatabaseRef{
		Host: "files-master", IsMaster: true,
		ApplicationMap: map[string]bool{"files": true},
	}
	defaultReplica := &DatabaseRef{Host: "default-replica", IsDefaultPartition: true}
	legacyReplica := &DatabaseRef{Host: "legacy-replica"}
	cc := &ClusterConfig{
		masters:  []*DatabaseRef{filesMaster},
		replicas: []*DatabaseRef{defaultReplica, legacyReplica},
	}

	if got := cc.GetReplicaForApplication("files"); got != legacyReplica {
		t.Fatalf("routed replica = %v, want legacy fallback", got)
	}
	if cc.ServesApplication(defaultReplica, "files") {
		t.Fatal("default replica must not serve an explicit master partition")
	}
	if !cc.ServesApplication(legacyReplica, "files") {
		t.Fatal("legacy fallback replica should serve the explicit master partition")
	}
}

func TestGetMasterForApplicationNoMatch(t *testing.T) {
	cc := &ClusterConfig{masters: []*DatabaseRef{
		{Host: "m1", Disabled: true, IsDefaultPartition: true},
	}}
	if m := cc.GetMasterForApplication("x"); m != nil {
		t.Errorf("expected nil when all masters disabled, got %v", m)
	}
}

func TestGetReplicaForApplication(t *testing.T) {
	none := &ClusterConfig{masters: []*DatabaseRef{{Host: "m1", IsDefaultPartition: true}}}
	if r := none.GetReplicaForApplication("config"); r != nil {
		t.Errorf("expected nil replica, got %v", r)
	}

	withReplica := &ClusterConfig{
		masters:  []*DatabaseRef{{Host: "m1", IsDefaultPartition: true}},
		replicas: []*DatabaseRef{{Host: "r1"}},
	}
	if r := withReplica.GetReplicaForApplication("config"); r == nil || r.Host != "r1" {
		t.Errorf("expected r1, got %v", r)
	}

	skipsDisabled := &ClusterConfig{
		masters:  []*DatabaseRef{{Host: "m1", IsDefaultPartition: true}},
		replicas: []*DatabaseRef{{Host: "r1", Disabled: true}, {Host: "r2"}},
	}
	if r := skipsDisabled.GetReplicaForApplication("config"); r == nil || r.Host != "r2" {
		t.Errorf("expected r2 (skipping disabled), got %v", r)
	}

	partitioned := &ClusterConfig{
		masters: []*DatabaseRef{{Host: "files-master", ApplicationMap: map[string]bool{"files": true}}},
		replicas: []*DatabaseRef{
			{Host: "default-replica", IsDefaultPartition: true},
			{Host: "files-replica", ApplicationMap: map[string]bool{"files": true}},
		},
	}
	if r := partitioned.GetReplicaForApplication("files"); r == nil || r.Host != "files-replica" {
		t.Errorf("expected the replica from the files partition, got %v", r)
	}
}

func TestGetAllRefs(t *testing.T) {
	cc := &ClusterConfig{Refs: []*DatabaseRef{{Host: "a"}, {Host: "b"}}}
	if got := cc.GetAllRefs(); len(got) != 2 {
		t.Errorf("expected 2 refs, got %d", len(got))
	}
}

func TestFirstNonEmptyAndZero(t *testing.T) {
	if got := firstNonEmpty("", "", "c"); got != "c" {
		t.Errorf("firstNonEmpty = %q, want c", got)
	}
	if got := firstNonEmpty("a", "b"); got != "a" {
		t.Errorf("firstNonEmpty = %q, want a", got)
	}
	if got := firstNonZero(0, 0, 5); got != 5 {
		t.Errorf("firstNonZero = %d, want 5", got)
	}
	if got := firstNonZero(3, 5); got != 3 {
		t.Errorf("firstNonZero = %d, want 3", got)
	}
}

func TestIsApplicationHost(t *testing.T) {
	ref := &DatabaseRef{ApplicationMap: map[string]bool{"files": true}}
	if !ref.IsApplicationHost("files") {
		t.Error("expected files to be an application host")
	}
	if ref.IsApplicationHost("config") {
		t.Error("config should not be an application host")
	}
}
