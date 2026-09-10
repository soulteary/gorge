// Package dbapi is the database-management domain: it reports on Phorge's MySQL
// cluster over HTTP. See docs/modules/dbapi.md.
package dbapi

import (
	"fmt"

	"github.com/soulteary/gorge/go/internal/platform/config"
)

const (
	DefaultListenAddr = ":8080"
	DefaultMySQLHost  = "127.0.0.1"
	DefaultMySQLPort  = 3306
	DefaultMySQLUser  = "root"
	DefaultNamespace  = "phorge"
)

type Config struct {
	config.Base
	MySQLHost string
	MySQLPort int
	MySQLUser string
	MySQLPass string
	Namespace string
	ConfigFile string
}

// LoadFromEnv accepts only the monorepo GORGE_DB_* contract. The old
// MYSQL_*/STORAGE_NAMESPACE/PHORGE_CONFIG spellings belonged to the standalone
// deployment and were removed once Phorge pinned Gorge as part of its stack.
func LoadFromEnv() *Config {
	return &Config{
		Base: config.LoadBase(DefaultListenAddr),
		MySQLHost: config.EnvStr(DefaultMySQLHost, "GORGE_DB_MYSQL_HOST"),
		MySQLPort: config.EnvInt(DefaultMySQLPort, "GORGE_DB_MYSQL_PORT"),
		MySQLUser: config.EnvStr(DefaultMySQLUser, "GORGE_DB_MYSQL_USER"),
		MySQLPass: config.EnvStr("", "GORGE_DB_MYSQL_PASS"),
		Namespace: config.EnvStr(DefaultNamespace, "GORGE_DB_NAMESPACE"),
		ConfigFile: config.EnvStr("", "GORGE_DB_CONFIG_FILE"),
	}
}

type ClusterConfig struct {
	Refs      []*DatabaseRef
	Namespace string
	MySQLPass string
	masters   []*DatabaseRef
	replicas  []*DatabaseRef
}

func (c *Config) BuildCluster() (*ClusterConfig, error) {
	if c.ConfigFile != "" {
		cc, err := loadClusterFromFile(c.ConfigFile, c)
		if err != nil {
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

func (cc *ClusterConfig) GetMasterForApplication(app string) *DatabaseRef {
	var appMaster, defaultMaster, unpartitionedMaster *DatabaseRef
	for _, m := range cc.masters {
		if m.Disabled { continue }
		if m.IsApplicationHost(app) { appMaster = m; break }
		if m.IsDefaultPartition && defaultMaster == nil { defaultMaster = m }
		if !m.IsDefaultPartition && len(m.ApplicationMap) == 0 && unpartitionedMaster == nil { unpartitionedMaster = m }
	}
	if appMaster != nil { return appMaster }
	if defaultMaster != nil { return defaultMaster }
	return unpartitionedMaster
}

func (cc *ClusterConfig) GetReplicaForApplication(app string) *DatabaseRef {
	var appReplica, defaultReplica, unpartitionedReplica *DatabaseRef
	master := cc.GetMasterForApplication(app)
	for _, r := range cc.replicas {
		if r.Disabled { continue }
		if r.IsApplicationHost(app) && appReplica == nil { appReplica = r }
		if r.IsDefaultPartition && defaultReplica == nil { defaultReplica = r }
		if !r.IsDefaultPartition && len(r.ApplicationMap) == 0 && unpartitionedReplica == nil { unpartitionedReplica = r }
	}
	if appReplica != nil { return appReplica }
	if master != nil && master.IsDefaultPartition && defaultReplica != nil { return defaultReplica }
	return unpartitionedReplica
}

func (cc *ClusterConfig) ServesApplication(ref *DatabaseRef, app string) bool {
	if ref == nil || ref.Disabled { return false }
	var refs []*DatabaseRef
	if ref.IsMaster { refs = cc.masters } else { refs = cc.replicas }
	hasExplicit := false
	hasDefault := false
	for _, candidate := range refs {
		if candidate.Disabled { continue }
		hasExplicit = hasExplicit || candidate.IsApplicationHost(app)
		hasDefault = hasDefault || candidate.IsDefaultPartition
	}
	if hasExplicit { return ref.IsApplicationHost(app) }
	if ref.IsMaster && hasDefault { return ref.IsDefaultPartition }
	if !ref.IsMaster {
		master := cc.GetMasterForApplication(app)
		if master != nil && master.IsDefaultPartition && hasDefault { return ref.IsDefaultPartition }
	}
	return !ref.IsDefaultPartition && len(ref.ApplicationMap) == 0
}

func (cc *ClusterConfig) GetAllRefs() []*DatabaseRef { return cc.Refs }
func (cc *ClusterConfig) Masters() []*DatabaseRef { return cc.masters }
func (cc *ClusterConfig) DatabaseName(app string) string { return cc.Namespace + "_" + app }
