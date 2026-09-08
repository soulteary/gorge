package dbapi

import (
	"github.com/soulteary/gorge/go/internal/platform/config"
)

// nodeSpec is one node of a Phorge `cluster.databases` array.
type nodeSpec struct {
	Host      string   `json:"host"`
	Port      int      `json:"port,omitempty"`
	User      string   `json:"user,omitempty"`
	Pass      string   `json:"pass,omitempty"`
	Role      string   `json:"role"`
	Disabled  bool     `json:"disabled,omitempty"`
	Partition []string `json:"partition,omitempty"`
}

// rawClusterFile is the subset of a Phorge local.json this service reads. The
// keys are Phorge's own dotted config names.
type rawClusterFile struct {
	MysqlHost  string     `json:"mysql.host"`
	MysqlPort  int        `json:"mysql.port"`
	MysqlUser  string     `json:"mysql.user"`
	MysqlPass  string     `json:"mysql.pass"`
	Namespace  string     `json:"storage.default-namespace"`
	ClusterDBs []nodeSpec `json:"cluster.databases"`
}

// loadClusterFromFile parses a Phorge local.json into a ClusterConfig. The
// env-derived Config supplies the fallbacks for anything the file omits, so a
// file that lists only hosts still gets the user, port and namespace from the
// environment.
func loadClusterFromFile(path string, base *Config) (*ClusterConfig, error) {
	var raw rawClusterFile
	if err := config.LoadJSONFile(path, &raw); err != nil {
		return nil, err
	}
	return buildClusterFromRaw(raw, base), nil
}

func buildClusterFromRaw(raw rawClusterFile, base *Config) *ClusterConfig {
	ns := firstNonEmpty(raw.Namespace, base.Namespace, DefaultNamespace)
	pass := firstNonEmpty(raw.MysqlPass, base.MySQLPass)

	// No cluster.databases: the file is describing a single node through the
	// scalar mysql.* keys, so build one master the same way the env path does.
	if len(raw.ClusterDBs) == 0 {
		ref := &DatabaseRef{
			Host:               firstNonEmpty(raw.MysqlHost, base.MySQLHost, DefaultMySQLHost),
			Port:               firstNonZero(raw.MysqlPort, base.MySQLPort, DefaultMySQLPort),
			User:               firstNonEmpty(raw.MysqlUser, base.MySQLUser, DefaultMySQLUser),
			Password:           pass,
			IsMaster:           true,
			IsIndividual:       true,
			IsDefaultPartition: true,
		}
		return &ClusterConfig{
			Refs:      []*DatabaseRef{ref},
			Namespace: ns,
			MySQLPass: pass,
			masters:   []*DatabaseRef{ref},
		}
	}

	var refs, masters, replicas []*DatabaseRef
	for _, spec := range raw.ClusterDBs {
		ref := &DatabaseRef{
			Host:     firstNonEmpty(spec.Host, raw.MysqlHost, base.MySQLHost),
			Port:     firstNonZero(spec.Port, raw.MysqlPort, base.MySQLPort, DefaultMySQLPort),
			User:     firstNonEmpty(spec.User, raw.MysqlUser, base.MySQLUser),
			Password: firstNonEmpty(spec.Pass, pass),
			IsMaster: spec.Role == "master",
			Disabled: spec.Disabled,
		}
		if len(spec.Partition) > 0 {
			ref.ApplicationMap = make(map[string]bool, len(spec.Partition))
			for _, app := range spec.Partition {
				if app == "default" {
					ref.IsDefaultPartition = true
				} else {
					ref.ApplicationMap[app] = true
				}
			}
		} else if ref.IsMaster {
			// A master with no explicit partition serves the default one, the
			// same assumption Phorge makes.
			ref.IsDefaultPartition = true
		}
		refs = append(refs, ref)
		if ref.IsMaster {
			masters = append(masters, ref)
		} else {
			replicas = append(replicas, ref)
		}
	}

	return &ClusterConfig{
		Refs:      refs,
		Namespace: ns,
		MySQLPass: pass,
		masters:   masters,
		replicas:  replicas,
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonZero(values ...int) int {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}
