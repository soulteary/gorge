package dbapi

import (
	"fmt"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// DatabaseRef is one database node in the configured cluster, mirroring
// Phorge's PhabricatorDatabaseRef. It carries both the static topology (host,
// role, partition membership) and the live probe results a health check fills
// in.
//
// It is the domain's internal type; the wire shape is contracts.ServerRef,
// which toContract builds. Keeping them separate lets the probe fields stay
// snake-free Go while the JSON contract uses camelCase, and keeps the domain
// free to add fields the contract does not expose.
type DatabaseRef struct {
	Host               string
	Port               int
	User               string
	IsMaster           bool
	Disabled           bool
	IsIndividual       bool
	IsDefaultPartition bool
	ApplicationMap     map[string]bool

	ConnectionStatus  ConnectionStatus
	ConnectionLatency float64
	ConnectionMessage string

	ReplicaStatus  ReplicaStatus
	ReplicaMessage string
	ReplicaDelay   *int
}

// ConnectionStatus is the outcome of the connection half of a health probe.
type ConnectionStatus string

const (
	StatusOkay              ConnectionStatus = "okay"
	StatusFail              ConnectionStatus = "fail"
	StatusAuth              ConnectionStatus = "auth"
	StatusReplicationClient ConnectionStatus = "replication-client"
)

// ReplicaStatus is the outcome of the replication half of a health probe.
type ReplicaStatus string

const (
	ReplicationOkay           ReplicaStatus = "okay"
	ReplicationMasterReplica  ReplicaStatus = "master-replica"
	ReplicationReplicaNone    ReplicaStatus = "replica-none"
	ReplicationSlow           ReplicaStatus = "replica-slow"
	ReplicationNotReplicating ReplicaStatus = "not-replicating"
)

// RefKey is the node's stable identifier, host:port, matching Phorge's own key
// for the same server. A node without a port (which does not occur for MySQL
// but is defended against) is keyed by host alone.
func (r *DatabaseRef) RefKey() string {
	if r.Port > 0 {
		return fmt.Sprintf("%s:%d", r.Host, r.Port)
	}
	return r.Host
}

// IsApplicationHost reports whether this node is bound to the named Phorge
// application in the cluster partition map.
func (r *DatabaseRef) IsApplicationHost(app string) bool {
	return r.ApplicationMap[app]
}

// toContract projects the ref onto its wire shape. The probe fields are copied
// as-is; empty replica fields stay empty and are dropped by omitempty.
func (r *DatabaseRef) toContract() contracts.ServerRef {
	return contracts.ServerRef{
		RefKey:             r.RefKey(),
		Host:               r.Host,
		Port:               r.Port,
		User:               r.User,
		IsMaster:           r.IsMaster,
		Disabled:           r.Disabled,
		IsIndividual:       r.IsIndividual,
		IsDefaultPartition: r.IsDefaultPartition,
		ConnectionStatus:   string(r.ConnectionStatus),
		ConnectionLatency:  r.ConnectionLatency,
		ConnectionMessage:  r.ConnectionMessage,
		ReplicaStatus:      string(r.ReplicaStatus),
		ReplicaMessage:     r.ReplicaMessage,
		ReplicaDelay:       r.ReplicaDelay,
	}
}
