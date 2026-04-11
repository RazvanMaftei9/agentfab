package controlplane

import (
	"time"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

type NodeState string

const (
	NodeStateUnknown     NodeState = "unknown"
	NodeStateReady       NodeState = "ready"
	NodeStateDegraded    NodeState = "degraded"
	NodeStateDraining    NodeState = "draining"
	NodeStateUnavailable NodeState = "unavailable"
)

type InstanceState string

const (
	InstanceStateUnknown     InstanceState = "unknown"
	InstanceStateStarting    InstanceState = "starting"
	InstanceStateReady       InstanceState = "ready"
	InstanceStateBusy        InstanceState = "busy"
	InstanceStateDraining    InstanceState = "draining"
	InstanceStateUnavailable InstanceState = "unavailable"
)

type RequestState string

const (
	RequestStatePending     RequestState = "pending"
	RequestStateRunning     RequestState = "running"
	RequestStateCompleted   RequestState = "completed"
	RequestStateFailed      RequestState = "failed"
	RequestStateCancelled   RequestState = "cancelled"
	RequestStateInterrupted RequestState = "interrupted"
)

type ProfileScaling struct {
	MinInstances int
	MaxInstances int
}

type AgentProfile struct {
	Name               string
	Definition         runtime.AgentDefinition
	RequiredNodeLabels map[string]string
	Scaling            ProfileScaling
}

type NodeCapacity struct {
	MaxInstances int
	MaxTasks     int
}

type Node struct {
	ID              string
	Address         string
	State           NodeState
	Labels          map[string]string
	BundleDigest    string
	ProfileDigests  map[string]string
	Capacity        NodeCapacity
	StartedAt       time.Time
	LastHeartbeatAt time.Time
}

type AgentInstance struct {
	ID              string
	Profile         string
	NodeID          string
	BundleDigest    string
	ProfileDigest   string
	Endpoint        runtime.Endpoint
	State           InstanceState
	StartedAt       time.Time
	LastHeartbeatAt time.Time
}

type AttestedNodeRecord struct {
	NodeID       string
	TrustDomain  string
	Claims       map[string]string
	Measurements map[string]string
	AttestedAt   time.Time
	ExpiresAt    time.Time
}

type LeaderLease struct {
	Fabric          string
	HolderID        string
	HolderAddress   string
	Epoch           int64
	AcquiredAt      time.Time
	LastHeartbeatAt time.Time
	ExpiresAt       time.Time
}

type RequestRecord struct {
	ID           string
	State        RequestState
	UserRequest  string
	GraphVersion int64
	LeaderID     string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type TaskRecord struct {
	RequestID        string
	TaskID           string
	Profile          string
	AssignedInstance string
	ExecutionNode    string
	Status           string
	LeaseEpoch       int64
	UpdatedAt        time.Time
}

type TaskLease struct {
	RequestID        string
	TaskID           string
	Profile          string
	AssignedInstance string
	ExecutionNode    string
	OwnerID          string
	Epoch            int64
	AcquiredAt       time.Time
	LastHeartbeatAt  time.Time
	ExpiresAt        time.Time
}

type ProfileLease struct {
	Profile          string
	AssignedInstance string
	ExecutionNode    string
	OwnerID          string
	Epoch            int64
	AcquiredAt       time.Time
	LastHeartbeatAt  time.Time
	ExpiresAt        time.Time
}

type InstanceFilter struct {
	Profile string
	NodeID  string
	State   InstanceState
}
