package dbapi

import (
	"encoding/json"
	"fmt"

	"github.com/soulteary/gorge/go/internal/platform/config"
)

// stringList accepts both Phorge's list form and the scalar form used by
// older/forked local.json files.
type stringList []string

func (s *stringList) UnmarshalJSON(data []byte) error {
	var values []string
	if err := json.Unmarshal(data, &values); err == nil {
		*s = values
		return nil
	}

	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("partition must be a string or string list: %w", err)
	}
	if value == "" {
		*s = nil
	} else {
		*s = []string{value}
	}
	return nil
}

// nodeSpec is one node of a Phorge `cluster.databases` array.
type nodeSpec struct {
	Host      string          `json:"host"`
	Port      int             `json:"port,omitempty"`
	User      string          `json:"user,omitempty"`
	Pass      *string         `json:"pass,omitempty"`
	Role      string          `json:"role"`
	Roles     map[string]bool `json:"roles,omitempty"`
	Disabled  bool            `json:"disabled,omitempty"`
	Partition stringList      `json:"partition,omitempty"`
}

func (s nodeSpec) role() string {
	if s.Role != "" {
		return s.Role
	}
	if s.Roles["master"] {
		return "master"
	}
	if s.Roles["replica"] {
		return "replica"
	}
	return ""
}

// rawClusterFile is the subset of a Phorge local.json this service reads. The
// keys are Phorge's own dotted config names.
type rawClusterFile struct {
	MysqlHost  string     `json:"mysql.host"`
	MysqlPort  int        `json:"mysql.port"`
	MysqlUser  string     `json:"mysql.user"`
	MysqlPass  *string    `json:"mysql.pass"`
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
	pass := base.MySQLPass
	if raw.MysqlPass != nil {
		pass = *raw.MysqlPass
	}

	// No cluster.databases: the file is describing a single node through the
	// scalar mysql.* keys, so build one master the same way the env path does.
	if len(raw.ClusterDBs) == 0 {
		ref := &DatabaseRef{
			Host:               firstNonEmpty(raw.MysqlHost, base.MySQLHost, DefaultMySQLHost),
			Port:               firstNonZero(raw.MysqlPort, base.MySQLPort, DefaultMySQLPort),
			User:               firstNonEmpty(raw.MysqlUser, base.MySQLUser, DefaultMySQLUser),
			Password:           pass,
			PasswordSet:        raw.MysqlPass != nil,
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
		password := pass
		passwordSet := false
		if spec.Pass != nil {
			password = *spec.Pass
			passwordSet = true
		}
		ref := &DatabaseRef{
			Host:        firstNonEmpty(spec.Host, raw.MysqlHost, base.MySQLHost),
			Port:        firstNonZero(spec.Port, raw.MysqlPort, base.MySQLPort, DefaultMySQLPort),
			User:        firstNonEmpty(spec.User, raw.MysqlUser, base.MySQLUser),
			Password:    password,
			PasswordSet: passwordSet,
			IsMaster:    spec.role() == "master",
			Disabled:    spec.Disabled,
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
