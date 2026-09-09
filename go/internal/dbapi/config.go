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
	"fmt"

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
// the scalar settings.
//
// The two paths fail differently on purpose. The single-node path is the
// stock deployment and always succeeds: a scalar host is all it needs, and an
// unreachable host is a readiness problem, not a boot one. But when
// GORGE_DB_CONFIG_FILE names a file, the operator is describing a real
// cluster, and every failure mode of that description — the file is missing,
// its JSON is malformed, a field has the wrong type, or it parses but names no
// usable master — is a misconfiguration the operator must see, not something
// to paper over. Silently degrading such a deployment to a single node built
// from the scalar fallbacks would route every application at one host, hide
// the replicas from every report, and read as healthy while doing it. So the
// file path fails closed: BuildCluster returns a configuration error and
// main.go stops the boot. The single-node fallback is reachable only when the
// variable is unset.
func (c *Config) BuildCluster() (*ClusterConfig, error) {
	if c.ConfigFile != "" {
		cc, err := loadClusterFromFile(c.ConfigFile, c)
		if err != nil {
			// The path is included because it is operator-supplied
			// configuration, not cluster data: naming which file failed to
			// load is exactly what the operator needs, and it exposes no host,
			// database or credential.
			return nil, fmt.Errorf("load cluster configuration from %q: %w", c.ConfigFile, err)
		}
		if len(cc.masters) == 0 {
			return nil, fmt.Errorf(
				"cluster configuration %q defines no usable master; every deployment needs at least one node with the master role",
				c.ConfigFile)
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

// ServesApplication reports whether ref belongs to the partition the router
// would select for an application. Every ref in the winning partition is
// included, not only the first ref returned by GetMaster/GetReplica.
func (cc *ClusterConfig) ServesApplication(ref *DatabaseRef, app string) bool {
	if ref == nil || ref.Disabled {
		return false
	}

	var refs []*DatabaseRef
	if ref.IsMaster {
		refs = cc.masters
	} else {
		refs = cc.replicas
	}

	hasExplicit := false
	hasDefault := false
	for _, candidate := range refs {
		if candidate.Disabled {
			continue
		}
		hasExplicit = hasExplicit || candidate.IsApplicationHost(app)
		hasDefault = hasDefault || candidate.IsDefaultPartition
	}
	if hasExplicit {
		return ref.IsApplicationHost(app)
	}
	if ref.IsMaster && hasDefault {
		return ref.IsDefaultPartition
	}
	if !ref.IsMaster {
		master := cc.GetMasterForApplication(app)
		if master != nil && master.IsDefaultPartition && hasDefault {
			return ref.IsDefaultPartition
		}
	}
	return !ref.IsDefaultPartition && len(ref.ApplicationMap) == 0
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
