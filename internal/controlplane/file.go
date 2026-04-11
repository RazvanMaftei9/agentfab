package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type FileStore struct {
	mu              sync.RWMutex
	path            string
	lockPath        string
	fabric          string
	leader          *LeaderLease
	leaderEpoch     int64
	attestedNodes   map[string]AttestedNodeRecord
	nodes           map[string]Node
	instances       map[string]AgentInstance
	requests        map[string]RequestRecord
	tasks           map[string]map[string]TaskRecord
	profileLeases   map[string]ProfileLease
	profileEpochs   map[string]int64
	leases          map[string]map[string]TaskLease
	taskLeaseEpochs map[string]map[string]int64
}

type fileStoreState struct {
	Fabric          string                           `json:"fabric"`
	Leader          *LeaderLease                     `json:"leader,omitempty"`
	LeaderEpoch     int64                            `json:"leader_epoch"`
	AttestedNodes   map[string]AttestedNodeRecord    `json:"attested_nodes,omitempty"`
	Nodes           map[string]Node                  `json:"nodes,omitempty"`
	Instances       map[string]AgentInstance         `json:"instances,omitempty"`
	Requests        map[string]RequestRecord         `json:"requests,omitempty"`
	TaskRecords     map[string]map[string]TaskRecord `json:"task_records,omitempty"`
	ProfileLeases   map[string]ProfileLease          `json:"profile_leases,omitempty"`
	ProfileEpochs   map[string]int64                 `json:"profile_epochs,omitempty"`
	TaskLeases      map[string]map[string]TaskLease  `json:"task_leases,omitempty"`
	TaskLeaseEpochs map[string]map[string]int64      `json:"task_lease_epochs,omitempty"`
}

func NewFileStore(baseDir, fabric string) (*FileStore, error) {
	dir := filepath.Join(baseDir, "shared", "controlplane")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create control plane dir: %w", err)
	}

	store := &FileStore{
		path:            filepath.Join(dir, "state.json"),
		lockPath:        filepath.Join(dir, "state.lock"),
		fabric:          fabric,
		attestedNodes:   make(map[string]AttestedNodeRecord),
		nodes:           make(map[string]Node),
		instances:       make(map[string]AgentInstance),
		requests:        make(map[string]RequestRecord),
		tasks:           make(map[string]map[string]TaskRecord),
		profileLeases:   make(map[string]ProfileLease),
		profileEpochs:   make(map[string]int64),
		leases:          make(map[string]map[string]TaskLease),
		taskLeaseEpochs: make(map[string]map[string]int64),
	}

	if err := store.withFileLock(func() error {
		return store.loadLocked()
	}); err != nil {
		return nil, err
	}

	return store, nil
}

func (s *FileStore) withFileLock(fn func() error) error {
	lockFile, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open control plane lock: %w", err)
	}
	defer lockFile.Close()

	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock control plane state: %w", err)
	}
	defer func() {
		_ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
	}()

	return fn()
}

func (s *FileStore) loadLocked() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.leader = nil
			s.leaderEpoch = 0
			s.attestedNodes = make(map[string]AttestedNodeRecord)
			s.nodes = make(map[string]Node)
			s.instances = make(map[string]AgentInstance)
			s.requests = make(map[string]RequestRecord)
			s.tasks = make(map[string]map[string]TaskRecord)
			s.profileLeases = make(map[string]ProfileLease)
			s.profileEpochs = make(map[string]int64)
			s.leases = make(map[string]map[string]TaskLease)
			s.taskLeaseEpochs = make(map[string]map[string]int64)
			return nil
		}
		return fmt.Errorf("read control plane state: %w", err)
	}

	var state fileStoreState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode control plane state: %w", err)
	}
	if state.Fabric != "" && s.fabric != "" && state.Fabric != s.fabric {
		return fmt.Errorf("control plane store fabric mismatch: got %q, want %q", state.Fabric, s.fabric)
	}
	if state.Fabric != "" {
		s.fabric = state.Fabric
	}
	s.leader = state.Leader
	s.leaderEpoch = state.LeaderEpoch
	s.attestedNodes = state.AttestedNodes
	s.nodes = state.Nodes
	s.instances = state.Instances
	s.requests = state.Requests
	s.tasks = state.TaskRecords
	s.profileLeases = state.ProfileLeases
	s.profileEpochs = state.ProfileEpochs
	s.leases = state.TaskLeases
	s.taskLeaseEpochs = state.TaskLeaseEpochs
	s.ensureMaps()
	return nil
}

func (s *FileStore) ensureMaps() {
	if s.nodes == nil {
		s.nodes = make(map[string]Node)
	}
	if s.attestedNodes == nil {
		s.attestedNodes = make(map[string]AttestedNodeRecord)
	}
	if s.instances == nil {
		s.instances = make(map[string]AgentInstance)
	}
	if s.requests == nil {
		s.requests = make(map[string]RequestRecord)
	}
	if s.tasks == nil {
		s.tasks = make(map[string]map[string]TaskRecord)
	}
	if s.profileLeases == nil {
		s.profileLeases = make(map[string]ProfileLease)
	}
	if s.profileEpochs == nil {
		s.profileEpochs = make(map[string]int64)
	}
	if s.leases == nil {
		s.leases = make(map[string]map[string]TaskLease)
	}
	if s.taskLeaseEpochs == nil {
		s.taskLeaseEpochs = make(map[string]map[string]int64)
	}
}

func (s *FileStore) saveLocked() error {
	s.ensureMaps()

	state := fileStoreState{
		Fabric:          s.fabric,
		Leader:          s.leader,
		LeaderEpoch:     s.leaderEpoch,
		AttestedNodes:   s.attestedNodes,
		Nodes:           s.nodes,
		Instances:       s.instances,
		Requests:        s.requests,
		TaskRecords:     s.tasks,
		ProfileLeases:   s.profileLeases,
		ProfileEpochs:   s.profileEpochs,
		TaskLeases:      s.leases,
		TaskLeaseEpochs: s.taskLeaseEpochs,
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode control plane state: %w", err)
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(s.path), "state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp control plane state: %w", err)
	}

	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp control plane state: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sync temp control plane state: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp control plane state: %w", err)
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod temp control plane state: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace control plane state: %w", err)
	}
	return nil
}

func (s *FileStore) withState(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.withFileLock(func() error {
		if err := s.loadLocked(); err != nil {
			return err
		}
		return fn()
	})
}

func (s *FileStore) withMutableState(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.withFileLock(func() error {
		if err := s.loadLocked(); err != nil {
			return err
		}
		if err := fn(); err != nil {
			return err
		}
		return s.saveLocked()
	})
}

func withStateValue[T any](s *FileStore, fn func() (T, error)) (T, error) {
	var zero T
	var value T

	if err := s.withState(func() error {
		var err error
		value, err = fn()
		return err
	}); err != nil {
		return zero, err
	}

	return value, nil
}

func (s *FileStore) AcquireLeader(_ context.Context, candidateID, candidateAddress string, ttl time.Duration) (LeaderLease, bool, error) {
	if candidateID == "" {
		return LeaderLease{}, false, fmt.Errorf("candidate ID is required")
	}
	if ttl <= 0 {
		return LeaderLease{}, false, fmt.Errorf("leader TTL must be positive")
	}

	now := time.Now()
	var lease LeaderLease
	acquired := false

	err := s.withMutableState(func() error {
		if s.leader != nil && s.leader.ExpiresAt.After(now) && s.leader.HolderID != candidateID {
			lease = *s.leader
			return nil
		}

		lease = LeaderLease{
			Fabric:          s.fabric,
			HolderID:        candidateID,
			HolderAddress:   candidateAddress,
			Epoch:           s.leaderEpoch + 1,
			AcquiredAt:      now,
			LastHeartbeatAt: now,
			ExpiresAt:       now.Add(ttl),
		}
		s.leader = &lease
		s.leaderEpoch = lease.Epoch
		acquired = true
		return nil
	})
	return lease, acquired, err
}

func (s *FileStore) RenewLeader(_ context.Context, lease LeaderLease, ttl time.Duration) (LeaderLease, error) {
	if ttl <= 0 {
		return LeaderLease{}, fmt.Errorf("leader TTL must be positive")
	}

	now := time.Now()
	var renewed LeaderLease

	err := s.withMutableState(func() error {
		if s.leader == nil {
			return fmt.Errorf("no leader lease")
		}
		if s.leader.HolderID != lease.HolderID || s.leader.Epoch != lease.Epoch {
			return fmt.Errorf("leader lease is no longer owned by %q", lease.HolderID)
		}
		if s.leader.ExpiresAt.Before(now) {
			return fmt.Errorf("leader lease expired")
		}

		s.leader.LastHeartbeatAt = now
		s.leader.ExpiresAt = now.Add(ttl)
		renewed = *s.leader
		return nil
	})
	return renewed, err
}

func (s *FileStore) GetLeader(_ context.Context) (LeaderLease, bool, error) {
	now := time.Now()
	var leader LeaderLease
	ok := false

	err := s.withMutableState(func() error {
		if s.leader == nil {
			return nil
		}
		if s.leader.ExpiresAt.Before(now) {
			s.leader = nil
			return nil
		}
		leader = *s.leader
		ok = true
		return nil
	})
	return leader, ok, err
}

func (s *FileStore) UpsertAttestedNode(_ context.Context, record AttestedNodeRecord) error {
	if record.NodeID == "" {
		return fmt.Errorf("attested node ID is required")
	}
	return s.withMutableState(func() error {
		s.attestedNodes[record.NodeID] = cloneAttestedNodeRecord(record)
		return nil
	})
}

func (s *FileStore) GetAttestedNode(_ context.Context, nodeID string) (AttestedNodeRecord, bool, error) {
	type result struct {
		record AttestedNodeRecord
		ok     bool
	}

	value, err := withStateValue(s, func() (result, error) {
		record, ok := s.attestedNodes[nodeID]
		if !ok {
			return result{}, nil
		}
		return result{record: cloneAttestedNodeRecord(record), ok: true}, nil
	})
	if err != nil {
		return AttestedNodeRecord{}, false, err
	}
	return value.record, value.ok, nil
}

func (s *FileStore) ReleaseLeader(_ context.Context, lease LeaderLease) error {
	return s.withMutableState(func() error {
		if s.leader == nil {
			return nil
		}
		if s.leader.HolderID != lease.HolderID || s.leader.Epoch != lease.Epoch {
			return nil
		}
		s.leader = nil
		return nil
	})
}

func (s *FileStore) RegisterNode(_ context.Context, node Node) error {
	if node.ID == "" {
		return fmt.Errorf("node ID is required")
	}
	if node.State == "" {
		node.State = NodeStateReady
	}
	if node.StartedAt.IsZero() {
		node.StartedAt = time.Now()
	}
	if node.LastHeartbeatAt.IsZero() {
		node.LastHeartbeatAt = node.StartedAt
	}

	return s.withMutableState(func() error {
		normalizeMembershipLiveness(s.nodes, s.instances, time.Now())
		s.nodes[node.ID] = cloneNode(node)
		return nil
	})
}

func (s *FileStore) HeartbeatNode(_ context.Context, nodeID string, at time.Time) error {
	if nodeID == "" {
		return fmt.Errorf("node ID is required")
	}
	if at.IsZero() {
		at = time.Now()
	}

	return s.withMutableState(func() error {
		normalizeMembershipLiveness(s.nodes, s.instances, time.Now())
		node, ok := s.nodes[nodeID]
		if !ok {
			return fmt.Errorf("node %q not found", nodeID)
		}
		node.LastHeartbeatAt = at
		if node.State == "" || node.State == NodeStateUnknown || node.State == NodeStateUnavailable {
			node.State = NodeStateReady
		}
		s.nodes[nodeID] = node
		return nil
	})
}

func (s *FileStore) GetNode(_ context.Context, nodeID string) (Node, bool, error) {
	type result struct {
		node Node
		ok   bool
	}

	value, err := withStateValue(s, func() (result, error) {
		normalizeMembershipLiveness(s.nodes, s.instances, time.Now())
		node, ok := s.nodes[nodeID]
		if !ok {
			return result{}, nil
		}
		return result{node: cloneNode(node), ok: true}, nil
	})
	return value.node, value.ok, err
}

func (s *FileStore) ListNodes(_ context.Context) ([]Node, error) {
	return withStateValue(s, func() ([]Node, error) {
		normalizeMembershipLiveness(s.nodes, s.instances, time.Now())
		nodes := make([]Node, 0, len(s.nodes))
		for _, node := range s.nodes {
			nodes = append(nodes, cloneNode(node))
		}
		sort.Slice(nodes, func(i, j int) bool {
			return nodes[i].ID < nodes[j].ID
		})
		return nodes, nil
	})
}

func (s *FileStore) RegisterInstance(_ context.Context, instance AgentInstance) error {
	if instance.ID == "" {
		return fmt.Errorf("instance ID is required")
	}
	if instance.Profile == "" {
		return fmt.Errorf("instance profile is required")
	}
	if instance.State == "" {
		instance.State = InstanceStateReady
	}
	if instance.StartedAt.IsZero() {
		instance.StartedAt = time.Now()
	}
	if instance.LastHeartbeatAt.IsZero() {
		instance.LastHeartbeatAt = instance.StartedAt
	}

	return s.withMutableState(func() error {
		normalizeMembershipLiveness(s.nodes, s.instances, time.Now())
		s.instances[instance.ID] = instance
		return nil
	})
}

func (s *FileStore) HeartbeatInstance(_ context.Context, instanceID string, at time.Time) error {
	if instanceID == "" {
		return fmt.Errorf("instance ID is required")
	}
	if at.IsZero() {
		at = time.Now()
	}

	return s.withMutableState(func() error {
		normalizeMembershipLiveness(s.nodes, s.instances, time.Now())
		instance, ok := s.instances[instanceID]
		if !ok {
			return fmt.Errorf("instance %q not found", instanceID)
		}
		instance.LastHeartbeatAt = at
		if instance.State == "" || instance.State == InstanceStateUnknown || instance.State == InstanceStateUnavailable {
			instance.State = InstanceStateReady
		}
		s.instances[instanceID] = instance
		return nil
	})
}

func (s *FileStore) GetInstance(_ context.Context, instanceID string) (AgentInstance, bool, error) {
	type result struct {
		instance AgentInstance
		ok       bool
	}

	value, err := withStateValue(s, func() (result, error) {
		normalizeMembershipLiveness(s.nodes, s.instances, time.Now())
		instance, ok := s.instances[instanceID]
		if !ok {
			return result{}, nil
		}
		return result{instance: instance, ok: true}, nil
	})
	return value.instance, value.ok, err
}

func (s *FileStore) ListInstances(_ context.Context, filter InstanceFilter) ([]AgentInstance, error) {
	return withStateValue(s, func() ([]AgentInstance, error) {
		normalizeMembershipLiveness(s.nodes, s.instances, time.Now())
		instances := make([]AgentInstance, 0, len(s.instances))
		for _, instance := range s.instances {
			if filter.Profile != "" && instance.Profile != filter.Profile {
				continue
			}
			if filter.NodeID != "" && instance.NodeID != filter.NodeID {
				continue
			}
			if filter.State != "" && instance.State != filter.State {
				continue
			}
			instances = append(instances, instance)
		}
		sort.Slice(instances, func(i, j int) bool {
			return instances[i].ID < instances[j].ID
		})
		return instances, nil
	})
}

func (s *FileStore) RemoveInstance(_ context.Context, instanceID string) error {
	return s.withMutableState(func() error {
		delete(s.instances, instanceID)
		return nil
	})
}

func (s *FileStore) UpsertRequest(_ context.Context, request RequestRecord) error {
	if request.ID == "" {
		return fmt.Errorf("request ID is required")
	}

	now := time.Now()

	return s.withMutableState(func() error {
		if request.CreatedAt.IsZero() {
			if existing, ok := s.requests[request.ID]; ok {
				request.CreatedAt = existing.CreatedAt
			} else {
				request.CreatedAt = now
			}
		}
		request.UpdatedAt = now
		s.requests[request.ID] = request
		return nil
	})
}

func (s *FileStore) GetRequest(_ context.Context, requestID string) (RequestRecord, bool, error) {
	type result struct {
		request RequestRecord
		ok      bool
	}

	value, err := withStateValue(s, func() (result, error) {
		request, ok := s.requests[requestID]
		if !ok {
			return result{}, nil
		}
		return result{request: request, ok: true}, nil
	})
	return value.request, value.ok, err
}

func (s *FileStore) ListRequests(_ context.Context) ([]RequestRecord, error) {
	return withStateValue(s, func() ([]RequestRecord, error) {
		requests := make([]RequestRecord, 0, len(s.requests))
		for _, request := range s.requests {
			requests = append(requests, request)
		}
		sort.Slice(requests, func(i, j int) bool {
			return requests[i].ID < requests[j].ID
		})
		return requests, nil
	})
}

func (s *FileStore) UpsertTask(_ context.Context, task TaskRecord) error {
	if task.RequestID == "" {
		return fmt.Errorf("task request ID is required")
	}
	if task.TaskID == "" {
		return fmt.Errorf("task ID is required")
	}

	task.UpdatedAt = time.Now()

	return s.withMutableState(func() error {
		if s.tasks[task.RequestID] == nil {
			s.tasks[task.RequestID] = make(map[string]TaskRecord)
		}
		s.tasks[task.RequestID][task.TaskID] = task
		return nil
	})
}

func (s *FileStore) ListTasks(_ context.Context, requestID string) ([]TaskRecord, error) {
	return withStateValue(s, func() ([]TaskRecord, error) {
		taskMap := s.tasks[requestID]
		tasks := make([]TaskRecord, 0, len(taskMap))
		for _, task := range taskMap {
			tasks = append(tasks, task)
		}
		sort.Slice(tasks, func(i, j int) bool {
			return tasks[i].TaskID < tasks[j].TaskID
		})
		return tasks, nil
	})
}

func (s *FileStore) AcquireTaskLease(_ context.Context, lease TaskLease, ttl time.Duration) (TaskLease, bool, error) {
	if lease.RequestID == "" {
		return TaskLease{}, false, fmt.Errorf("task lease request ID is required")
	}
	if lease.TaskID == "" {
		return TaskLease{}, false, fmt.Errorf("task lease task ID is required")
	}
	if lease.OwnerID == "" {
		return TaskLease{}, false, fmt.Errorf("task lease owner ID is required")
	}
	if ttl <= 0 {
		return TaskLease{}, false, fmt.Errorf("task lease TTL must be positive")
	}

	now := time.Now()
	acquired := false

	err := s.withMutableState(func() error {
		if s.leases[lease.RequestID] == nil {
			s.leases[lease.RequestID] = make(map[string]TaskLease)
		}
		if s.taskLeaseEpochs[lease.RequestID] == nil {
			s.taskLeaseEpochs[lease.RequestID] = make(map[string]int64)
		}

		existing, ok := s.leases[lease.RequestID][lease.TaskID]
		if ok && existing.ExpiresAt.After(now) && existing.OwnerID != lease.OwnerID {
			lease = existing
			return nil
		}

		lease.Epoch = s.taskLeaseEpochs[lease.RequestID][lease.TaskID] + 1
		lease.AcquiredAt = now
		lease.LastHeartbeatAt = now
		lease.ExpiresAt = now.Add(ttl)
		s.leases[lease.RequestID][lease.TaskID] = lease
		s.taskLeaseEpochs[lease.RequestID][lease.TaskID] = lease.Epoch
		acquired = true
		return nil
	})
	return lease, acquired, err
}

func (s *FileStore) AcquireProfileLease(_ context.Context, lease ProfileLease, ttl time.Duration) (ProfileLease, bool, error) {
	if lease.Profile == "" {
		return ProfileLease{}, false, fmt.Errorf("profile lease profile is required")
	}
	if lease.AssignedInstance == "" {
		return ProfileLease{}, false, fmt.Errorf("profile lease assigned instance is required")
	}
	if lease.OwnerID == "" {
		return ProfileLease{}, false, fmt.Errorf("profile lease owner ID is required")
	}
	if ttl <= 0 {
		return ProfileLease{}, false, fmt.Errorf("profile lease TTL must be positive")
	}

	now := time.Now()
	acquired := false

	err := s.withMutableState(func() error {
		existing, ok := s.profileLeases[lease.Profile]
		if ok && existing.ExpiresAt.After(now) {
			if existing.OwnerID == lease.OwnerID && existing.AssignedInstance == lease.AssignedInstance {
				existing.LastHeartbeatAt = now
				existing.ExpiresAt = now.Add(ttl)
				s.profileLeases[lease.Profile] = existing
				lease = existing
				acquired = true
				return nil
			}
			lease = existing
			return nil
		}

		lease.Epoch = s.profileEpochs[lease.Profile] + 1
		lease.AcquiredAt = now
		lease.LastHeartbeatAt = now
		lease.ExpiresAt = now.Add(ttl)
		s.profileLeases[lease.Profile] = lease
		s.profileEpochs[lease.Profile] = lease.Epoch
		acquired = true
		return nil
	})
	return lease, acquired, err
}

func (s *FileStore) RenewProfileLease(_ context.Context, lease ProfileLease, ttl time.Duration) (ProfileLease, error) {
	if ttl <= 0 {
		return ProfileLease{}, fmt.Errorf("profile lease TTL must be positive")
	}

	now := time.Now()
	var renewed ProfileLease

	err := s.withMutableState(func() error {
		current, ok := s.profileLeases[lease.Profile]
		if !ok {
			return fmt.Errorf("profile lease for %q not found", lease.Profile)
		}
		if current.OwnerID != lease.OwnerID || current.Epoch != lease.Epoch || current.AssignedInstance != lease.AssignedInstance {
			return fmt.Errorf("profile lease for %q is no longer owned by %q", lease.Profile, lease.OwnerID)
		}
		if current.ExpiresAt.Before(now) {
			return fmt.Errorf("profile lease expired")
		}

		current.LastHeartbeatAt = now
		current.ExpiresAt = now.Add(ttl)
		s.profileLeases[lease.Profile] = current
		renewed = current
		return nil
	})
	return renewed, err
}

func (s *FileStore) GetProfileLease(_ context.Context, profile string) (ProfileLease, bool, error) {
	now := time.Now()
	var lease ProfileLease
	ok := false

	err := s.withMutableState(func() error {
		current, exists := s.profileLeases[profile]
		if !exists {
			return nil
		}
		if current.ExpiresAt.Before(now) {
			delete(s.profileLeases, profile)
			return nil
		}
		lease = current
		ok = true
		return nil
	})
	return lease, ok, err
}

func (s *FileStore) ReleaseProfileLease(_ context.Context, lease ProfileLease) error {
	return s.withMutableState(func() error {
		current, ok := s.profileLeases[lease.Profile]
		if !ok {
			return nil
		}
		if current.OwnerID != lease.OwnerID || current.Epoch != lease.Epoch || current.AssignedInstance != lease.AssignedInstance {
			return nil
		}
		delete(s.profileLeases, lease.Profile)
		return nil
	})
}

func (s *FileStore) RenewTaskLease(_ context.Context, lease TaskLease, ttl time.Duration) (TaskLease, error) {
	if ttl <= 0 {
		return TaskLease{}, fmt.Errorf("task lease TTL must be positive")
	}

	now := time.Now()
	var renewed TaskLease

	err := s.withMutableState(func() error {
		requestLeases := s.leases[lease.RequestID]
		if requestLeases == nil {
			return fmt.Errorf("task lease for request %q not found", lease.RequestID)
		}

		current, ok := requestLeases[lease.TaskID]
		if !ok {
			return fmt.Errorf("task lease for task %q not found", lease.TaskID)
		}
		if current.OwnerID != lease.OwnerID || current.Epoch != lease.Epoch {
			return fmt.Errorf("task lease is no longer owned by %q", lease.OwnerID)
		}
		if current.ExpiresAt.Before(now) {
			return fmt.Errorf("task lease expired")
		}

		current.LastHeartbeatAt = now
		current.ExpiresAt = now.Add(ttl)
		requestLeases[lease.TaskID] = current
		renewed = current
		return nil
	})
	return renewed, err
}

func (s *FileStore) GetTaskLease(_ context.Context, requestID, taskID string) (TaskLease, bool, error) {
	now := time.Now()
	var lease TaskLease
	ok := false

	err := s.withMutableState(func() error {
		requestLeases := s.leases[requestID]
		if requestLeases == nil {
			return nil
		}
		current, exists := requestLeases[taskID]
		if !exists {
			return nil
		}
		if current.ExpiresAt.Before(now) {
			delete(requestLeases, taskID)
			if len(requestLeases) == 0 {
				delete(s.leases, requestID)
			}
			return nil
		}
		lease = current
		ok = true
		return nil
	})
	return lease, ok, err
}

func (s *FileStore) ReleaseTaskLease(_ context.Context, lease TaskLease) error {
	return s.withMutableState(func() error {
		requestLeases := s.leases[lease.RequestID]
		if requestLeases == nil {
			return nil
		}

		current, ok := requestLeases[lease.TaskID]
		if !ok {
			return nil
		}
		if current.OwnerID != lease.OwnerID || current.Epoch != lease.Epoch {
			return nil
		}

		delete(requestLeases, lease.TaskID)
		if len(requestLeases) == 0 {
			delete(s.leases, lease.RequestID)
		}
		return nil
	})
}
