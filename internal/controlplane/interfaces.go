package controlplane

import (
	"context"
	"time"
)

type Leadership interface {
	AcquireLeader(ctx context.Context, candidateID, candidateAddress string, ttl time.Duration) (LeaderLease, bool, error)
	RenewLeader(ctx context.Context, lease LeaderLease, ttl time.Duration) (LeaderLease, error)
	GetLeader(ctx context.Context) (LeaderLease, bool, error)
	ReleaseLeader(ctx context.Context, lease LeaderLease) error
}

type Membership interface {
	RegisterNode(ctx context.Context, node Node) error
	HeartbeatNode(ctx context.Context, nodeID string, at time.Time) error
	GetNode(ctx context.Context, nodeID string) (Node, bool, error)
	ListNodes(ctx context.Context) ([]Node, error)

	RegisterInstance(ctx context.Context, instance AgentInstance) error
	HeartbeatInstance(ctx context.Context, instanceID string, at time.Time) error
	GetInstance(ctx context.Context, instanceID string) (AgentInstance, bool, error)
	ListInstances(ctx context.Context, filter InstanceFilter) ([]AgentInstance, error)
	RemoveInstance(ctx context.Context, instanceID string) error
}

type MembershipWriter interface {
	RegisterNode(ctx context.Context, node Node) error
	HeartbeatNode(ctx context.Context, nodeID string, at time.Time) error
	RegisterInstance(ctx context.Context, instance AgentInstance) error
	HeartbeatInstance(ctx context.Context, instanceID string, at time.Time) error
	RemoveInstance(ctx context.Context, instanceID string) error
}

type Attestations interface {
	UpsertAttestedNode(ctx context.Context, record AttestedNodeRecord) error
	GetAttestedNode(ctx context.Context, nodeID string) (AttestedNodeRecord, bool, error)
}

type Requests interface {
	UpsertRequest(ctx context.Context, request RequestRecord) error
	GetRequest(ctx context.Context, requestID string) (RequestRecord, bool, error)
	ListRequests(ctx context.Context) ([]RequestRecord, error)

	UpsertTask(ctx context.Context, task TaskRecord) error
	ListTasks(ctx context.Context, requestID string) ([]TaskRecord, error)
}

type Leases interface {
	AcquireProfileLease(ctx context.Context, lease ProfileLease, ttl time.Duration) (ProfileLease, bool, error)
	RenewProfileLease(ctx context.Context, lease ProfileLease, ttl time.Duration) (ProfileLease, error)
	GetProfileLease(ctx context.Context, profile string) (ProfileLease, bool, error)
	ReleaseProfileLease(ctx context.Context, lease ProfileLease) error

	AcquireTaskLease(ctx context.Context, lease TaskLease, ttl time.Duration) (TaskLease, bool, error)
	RenewTaskLease(ctx context.Context, lease TaskLease, ttl time.Duration) (TaskLease, error)
	GetTaskLease(ctx context.Context, requestID, taskID string) (TaskLease, bool, error)
	ReleaseTaskLease(ctx context.Context, lease TaskLease) error
}

type Store interface {
	Leadership
	Membership
	Requests
	Leases
}
