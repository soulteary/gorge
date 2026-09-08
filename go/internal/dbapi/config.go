// Package dbapi is the database-management domain: it reports on Phorge's MySQL
// cluster over HTTP — which servers are configured, how healthy each is, how
// each server's schema and environment compare to what Phorge expects, and how
// far its migrations have run. It owns no state; every answer is derived live
// from the cluster it is pointed at. See docs/modules/dbapi.md.
//
// It is a port of the standalone gorge-db-api, with two deliberate changes: the
// HTTP layer is Fiber v3 + httpx like every other gorge domain, and the driver
// abstraction is dropped in favour of MySQL only. The domain algorithms —
// partition routing, read-only degradation, savepoint naming, the MySQL
// error-code tables, the three-tier INFORMATION_SCHEMA walk — are preserved.
package dbapi

import (
	"github.com/soulteary/gorge/go/internal/platform/config"
)

// Defaults mirror a stock single-node Phorge MySQL deployment. The listen
// address is the pre-monorepo :8080, which existing deployments point at.
const (
	DefaultListenAddr = ":8080"
	DefaultMySQLHost  = "127.0.0.1"
	DefaultMySQLPort  = 3306
	DefaultMySQLUser  = "root"
	DefaultNamespace  = "phorge"
)

// Config is the db-api service configuration.
//
// There are two ways to describe a cluster, and exactly one is used per boot:
// the individual GORGE_DB_MYSQL_* variables describe a single node, and a
// Phorge local.json named by GORGE_DB_CONFIG_FILE describes a full topology
// through its `cluster.databases`. The file wins when it is set, because a
// deployment that has one is running a real cluster and the scalar variables
// would only describe one node of it.
type Config struct {
	config.Base

	MySQLHost string
	MySQLPort int
	MySQLUser string
	MySQLPass string
	Namespace string

	// ConfigFile is a path to a Phorge-style local.json. When set, the cluster
	// topology is read from it and the MySQL* scalars above are used only as
	// fallbacks for nodes that omit a field.
	ConfigFile string
}

// LoadFromEnv reads the configuration from the environment. The GORGE_DB_ names
// are the current ones; the unprefixed names are what the pre-monorepo
// deployment used and are kept as a fallback, newest-name-first so a GORGE_DB_
// variable always wins over the legacy one. See docs/platform.md section 4.
func LoadFromEnv() *Config {
	return &Config{
		Base: config.LoadBase(DefaultListenAddr),

		MySQLHost: config.EnvStr(DefaultMySQLHost, "GORGE_DB_MYSQL_HOST", "MYSQL_HOST"),
		MySQLPort: config.EnvInt(DefaultMySQLPort, "GORGE_DB_MYSQL_PORT", "MYSQL_PORT"),
		MySQLUser: config.EnvStr(DefaultMySQLUser, "GORGE_DB_MYSQL_USER", "MYSQL_USER"),
		MySQLPass: config.EnvStr("", "GORGE_DB_MYSQL_PASS", "MYSQL_PASS"),
		Namespace: config.EnvStr(DefaultNamespace, "GORGE_DB_NAMESPACE", "STORAGE_NAMESPACE"),

		ConfigFile: config.EnvStr("", "GORGE_DB_CONFIG_FILE", "PHORGE_CONFIG"),
	}
}

// ClusterConfig is the parsed cluster topology, mirroring Phorge's
// PhabricatorDatabaseRefParser: the full ref list plus the master/replica
// split the router selects from, and the namespace that names the databases.
type ClusterConfig struct {
	Refs      []*DatabaseRef
	Namespace string
	MySQLPass string
	masters   []*DatabaseRef
	replicas  []*DatabaseRef
}

// BuildCluster turns a Config into a ClusterConfig. When ConfigFile is set it
// parses the Phorge local.json; otherwise it builds a single-node master from
// the scalar settings. A parse failure falls back to the single node so a bad
// file degrades to "one server" rather than a failed boot — the health probe
// then surfaces the real state.
func (c *Config) BuildCluster() (*ClusterConfig, error) {
	if c.ConfigFile != "" {
		cc, err := loadClusterFromFile(c.ConfigFile, c)
		if err != nil {
			return nil, err
		}
		return cc, nil
	}
	return c.singleNodeCluster(), nil
}

func (c *Config) singleNodeCluster() *ClusterConfig {
	ns := c.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	ref := &DatabaseRef{
		Host:               c.MySQLHost,
		Port:               c.MySQLPort,
		User:               c.MySQLUser,
		Password:           c.MySQLPass,
		IsMaster:           true,
		IsIndividual:       true,
		IsDefaultPartition: true,
	}
	return &ClusterConfig{
		Refs:      []*DatabaseRef{ref},
		Namespace: ns,
		MySQLPass: c.MySQLPass,
		masters:   []*DatabaseRef{ref},
	}
}

// GetMasterForApplication returns the master serving a Phorge application,
// preferring one explicitly bound to the application, then the default
// partition, then the legacy unpartitioned form. This is Phorge's
// application-partition routing, preserved so a partitioned deployment routes
// the same way Phorge does.
func (cc *ClusterConfig) GetMasterForApplication(app string) *DatabaseRef {
	var appMaster, defaultMaster, unpartitionedMaster *DatabaseRef
	for _, m := range cc.masters {
		if m.Disabled {
			continue
		}
		if m.IsApplicationHost(app) {
			appMaster = m
			break
		}
		if m.IsDefaultPartition && defaultMaster == nil {
			defaultMaster = m
		}
		if !m.IsDefaultPartition && len(m.ApplicationMap) == 0 && unpartitionedMaster == nil {
			unpartitionedMaster = m
		}
	}
	if appMaster != nil {
		return appMaster
	}
	if defaultMaster != nil {
		return defaultMaster
	}
	return unpartitionedMaster
}

// GetReplicaForApplication returns a replica for the application, matching the
// master's partition. Nil when the application has no replica.
func (cc *ClusterConfig) GetReplicaForApplication(app string) *DatabaseRef {
	var appReplica, defaultReplica, unpartitionedReplica *DatabaseRef
	master := cc.GetMasterForApplication(app)
	for _, r := range cc.replicas {
		if r.Disabled {
			continue
		}
		if r.IsApplicationHost(app) && appReplica == nil {
			appReplica = r
		}
		if r.IsDefaultPartition && defaultReplica == nil {
			defaultReplica = r
		}
		if !r.IsDefaultPartition && len(r.ApplicationMap) == 0 && unpartitionedReplica == nil {
			unpartitionedReplica = r
		}
	}
	if appReplica != nil {
		return appReplica
	}
	if master != nil && master.IsDefaultPartition && defaultReplica != nil {
		return defaultReplica
	}
	// Older cluster files often leave replicas unpartitioned. Treat such a
	// replica as a final generic fallback, but never let an explicitly default
	// replica satisfy an application-specific partition.
	return unpartitionedReplica
}

// GetAllRefs returns every configured node, enabled or not; callers skip the
// disabled ones themselves.
func (cc *ClusterConfig) GetAllRefs() []*DatabaseRef { return cc.Refs }

// Masters returns the master nodes, for the endpoints that only report on
// masters (migrations).
func (cc *ClusterConfig) Masters() []*DatabaseRef { return cc.masters }

// DatabaseName joins the namespace and a Phorge application into the database
// name Phorge uses, e.g. `phorge_meta_data`. It is the same derivation
// taskqueue uses for `{namespace}_worker`.
func (cc *ClusterConfig) DatabaseName(app string) string {
	return cc.Namespace + "_" + app
}
