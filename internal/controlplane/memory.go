package controlplane

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

type MemoryStore struct {
	mu              sync.RWMutex
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

func NewMemoryStore(fabric string) *MemoryStore {
	return &MemoryStore{
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
}

func (s *MemoryStore) UpsertAttestedNode(_ context.Context, record AttestedNodeRecord) error {
	if record.NodeID == "" {
		return fmt.Errorf("attested node ID is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attestedNodes[record.NodeID] = cloneAttestedNodeRecord(record)
	return nil
}

func (s *MemoryStore) GetAttestedNode(_ context.Context, nodeID string) (AttestedNodeRecord, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	record, ok := s.attestedNodes[nodeID]
	if !ok {
		return AttestedNodeRecord{}, false, nil
	}
	return cloneAttestedNodeRecord(record), true, nil
}

func (s *MemoryStore) AcquireLeader(_ context.Context, candidateID, candidateAddress string, ttl time.Duration) (LeaderLease, bool, error) {
	if candidateID == "" {
		return LeaderLease{}, false, fmt.Errorf("candidate ID is required")
	}
	if ttl <= 0 {
		return LeaderLease{}, false, fmt.Errorf("leader TTL must be positive")
	}

	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.leader != nil && s.leader.ExpiresAt.After(now) && s.leader.HolderID != candidateID {
		return *s.leader, false, nil
	}

	epoch := s.leaderEpoch + 1

	lease := LeaderLease{
		Fabric:          s.fabric,
		HolderID:        candidateID,
		HolderAddress:   candidateAddress,
		Epoch:           epoch,
		AcquiredAt:      now,
		LastHeartbeatAt: now,
		ExpiresAt:       now.Add(ttl),
	}
	s.leader = &lease
	s.leaderEpoch = epoch
	return lease, true, nil
}

func (s *MemoryStore) RenewLeader(_ context.Context, lease LeaderLease, ttl time.Duration) (LeaderLease, error) {
	if ttl <= 0 {
		return LeaderLease{}, fmt.Errorf("leader TTL must be positive")
	}

	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.leader == nil {
		return LeaderLease{}, fmt.Errorf("no leader lease")
	}
	if s.leader.HolderID != lease.HolderID || s.leader.Epoch != lease.Epoch {
		return LeaderLease{}, fmt.Errorf("leader lease is no longer owned by %q", lease.HolderID)
	}
	if s.leader.ExpiresAt.Before(now) {
		return LeaderLease{}, fmt.Errorf("leader lease expired")
	}

	s.leader.LastHeartbeatAt = now
	s.leader.ExpiresAt = now.Add(ttl)
	return *s.leader, nil
}

func (s *MemoryStore) GetLeader(_ context.Context) (LeaderLease, bool, error) {
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.leader == nil {
		return LeaderLease{}, false, nil
	}
	if s.leader.ExpiresAt.Before(now) {
		s.leader = nil
		return LeaderLease{}, false, nil
	}
	return *s.leader, true, nil
}

func (s *MemoryStore) ReleaseLeader(_ context.Context, lease LeaderLease) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.leader == nil {
		return nil
	}
	if s.leader.HolderID != lease.HolderID || s.leader.Epoch != lease.Epoch {
		return nil
	}
	s.leader = nil
	return nil
}

func (s *MemoryStore) RegisterNode(_ context.Context, node Node) error {
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

	s.mu.Lock()
	defer s.mu.Unlock()
	normalizeMembershipLiveness(s.nodes, s.instances, time.Now())
	s.nodes[node.ID] = cloneNode(node)
	return nil
}

func (s *MemoryStore) HeartbeatNode(_ context.Context, nodeID string, at time.Time) error {
	if nodeID == "" {
		return fmt.Errorf("node ID is required")
	}
	if at.IsZero() {
		at = time.Now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
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
}

func (s *MemoryStore) GetNode(_ context.Context, nodeID string) (Node, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	normalizeMembershipLiveness(s.nodes, s.instances, time.Now())

	node, ok := s.nodes[nodeID]
	if !ok {
		return Node{}, false, nil
	}
	return cloneNode(node), true, nil
}

func (s *MemoryStore) ListNodes(_ context.Context) ([]Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	normalizeMembershipLiveness(s.nodes, s.instances, time.Now())

	nodes := make([]Node, 0, len(s.nodes))
	for _, node := range s.nodes {
		nodes = append(nodes, cloneNode(node))
	}
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].ID < nodes[j].ID
	})
	return nodes, nil
}

func (s *MemoryStore) RegisterInstance(_ context.Context, instance AgentInstance) error {
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

	s.mu.Lock()
	defer s.mu.Unlock()
	normalizeMembershipLiveness(s.nodes, s.instances, time.Now())
	s.instances[instance.ID] = instance
	return nil
}

func (s *MemoryStore) HeartbeatInstance(_ context.Context, instanceID string, at time.Time) error {
	if instanceID == "" {
		return fmt.Errorf("instance ID is required")
	}
	if at.IsZero() {
		at = time.Now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
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
}

func (s *MemoryStore) GetInstance(_ context.Context, instanceID string) (AgentInstance, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	normalizeMembershipLiveness(s.nodes, s.instances, time.Now())

	instance, ok := s.instances[instanceID]
	if !ok {
		return AgentInstance{}, false, nil
	}
	return instance, true, nil
}

func (s *MemoryStore) ListInstances(_ context.Context, filter InstanceFilter) ([]AgentInstance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

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
}

func (s *MemoryStore) RemoveInstance(_ context.Context, instanceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.instances, instanceID)
	return nil
}

func (s *MemoryStore) UpsertRequest(_ context.Context, request RequestRecord) error {
	if request.ID == "" {
		return fmt.Errorf("request ID is required")
	}

	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

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
}

func (s *MemoryStore) GetRequest(_ context.Context, requestID string) (RequestRecord, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	request, ok := s.requests[requestID]
	if !ok {
		return RequestRecord{}, false, nil
	}
	return request, true, nil
}

func (s *MemoryStore) ListRequests(_ context.Context) ([]RequestRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	requests := make([]RequestRecord, 0, len(s.requests))
	for _, request := range s.requests {
		requests = append(requests, request)
	}
	sort.Slice(requests, func(i, j int) bool {
		return requests[i].ID < requests[j].ID
	})
	return requests, nil
}

func (s *MemoryStore) UpsertTask(_ context.Context, task TaskRecord) error {
	if task.RequestID == "" {
		return fmt.Errorf("task request ID is required")
	}
	if task.TaskID == "" {
		return fmt.Errorf("task ID is required")
	}

	task.UpdatedAt = time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.tasks[task.RequestID] == nil {
		s.tasks[task.RequestID] = make(map[string]TaskRecord)
	}
	s.tasks[task.RequestID][task.TaskID] = task
	return nil
}

func (s *MemoryStore) ListTasks(_ context.Context, requestID string) ([]TaskRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	taskMap := s.tasks[requestID]
	tasks := make([]TaskRecord, 0, len(taskMap))
	for _, task := range taskMap {
		tasks = append(tasks, task)
	}
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].TaskID < tasks[j].TaskID
	})
	return tasks, nil
}

func (s *MemoryStore) AcquireTaskLease(_ context.Context, lease TaskLease, ttl time.Duration) (TaskLease, bool, error) {
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

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.leases[lease.RequestID] == nil {
		s.leases[lease.RequestID] = make(map[string]TaskLease)
	}
	if s.taskLeaseEpochs[lease.RequestID] == nil {
		s.taskLeaseEpochs[lease.RequestID] = make(map[string]int64)
	}

	existing, ok := s.leases[lease.RequestID][lease.TaskID]
	if ok && existing.ExpiresAt.After(now) && existing.OwnerID != lease.OwnerID {
		return existing, false, nil
	}

	epoch := s.taskLeaseEpochs[lease.RequestID][lease.TaskID] + 1

	lease.Epoch = epoch
	lease.AcquiredAt = now
	lease.LastHeartbeatAt = now
	lease.ExpiresAt = now.Add(ttl)
	s.leases[lease.RequestID][lease.TaskID] = lease
	s.taskLeaseEpochs[lease.RequestID][lease.TaskID] = epoch
	return lease, true, nil
}

func (s *MemoryStore) AcquireProfileLease(_ context.Context, lease ProfileLease, ttl time.Duration) (ProfileLease, bool, error) {
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

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.profileLeases[lease.Profile]
	if ok && existing.ExpiresAt.After(now) {
		if existing.OwnerID == lease.OwnerID && existing.AssignedInstance == lease.AssignedInstance {
			existing.LastHeartbeatAt = now
			existing.ExpiresAt = now.Add(ttl)
			s.profileLeases[lease.Profile] = existing
			return existing, true, nil
		}
		return existing, false, nil
	}

	epoch := s.profileEpochs[lease.Profile] + 1
	lease.Epoch = epoch
	lease.AcquiredAt = now
	lease.LastHeartbeatAt = now
	lease.ExpiresAt = now.Add(ttl)
	s.profileLeases[lease.Profile] = lease
	s.profileEpochs[lease.Profile] = epoch
	return lease, true, nil
}

func (s *MemoryStore) RenewProfileLease(_ context.Context, lease ProfileLease, ttl time.Duration) (ProfileLease, error) {
	if ttl <= 0 {
		return ProfileLease{}, fmt.Errorf("profile lease TTL must be positive")
	}

	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.profileLeases[lease.Profile]
	if !ok {
		return ProfileLease{}, fmt.Errorf("profile lease for %q not found", lease.Profile)
	}
	if current.OwnerID != lease.OwnerID || current.Epoch != lease.Epoch || current.AssignedInstance != lease.AssignedInstance {
		return ProfileLease{}, fmt.Errorf("profile lease for %q is no longer owned by %q", lease.Profile, lease.OwnerID)
	}
	if current.ExpiresAt.Before(now) {
		return ProfileLease{}, fmt.Errorf("profile lease expired")
	}

	current.LastHeartbeatAt = now
	current.ExpiresAt = now.Add(ttl)
	s.profileLeases[lease.Profile] = current
	return current, nil
}

func (s *MemoryStore) GetProfileLease(_ context.Context, profile string) (ProfileLease, bool, error) {
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	lease, ok := s.profileLeases[profile]
	if !ok {
		return ProfileLease{}, false, nil
	}
	if lease.ExpiresAt.Before(now) {
		delete(s.profileLeases, profile)
		return ProfileLease{}, false, nil
	}
	return lease, true, nil
}

func (s *MemoryStore) ReleaseProfileLease(_ context.Context, lease ProfileLease) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.profileLeases[lease.Profile]
	if !ok {
		return nil
	}
	if current.OwnerID != lease.OwnerID || current.Epoch != lease.Epoch || current.AssignedInstance != lease.AssignedInstance {
		return nil
	}
	delete(s.profileLeases, lease.Profile)
	return nil
}

func (s *MemoryStore) RenewTaskLease(_ context.Context, lease TaskLease, ttl time.Duration) (TaskLease, error) {
	if ttl <= 0 {
		return TaskLease{}, fmt.Errorf("task lease TTL must be positive")
	}

	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	requestLeases := s.leases[lease.RequestID]
	if requestLeases == nil {
		return TaskLease{}, fmt.Errorf("task lease for request %q not found", lease.RequestID)
	}

	current, ok := requestLeases[lease.TaskID]
	if !ok {
		return TaskLease{}, fmt.Errorf("task lease for task %q not found", lease.TaskID)
	}
	if current.OwnerID != lease.OwnerID || current.Epoch != lease.Epoch {
		return TaskLease{}, fmt.Errorf("task lease is no longer owned by %q", lease.OwnerID)
	}
	if current.ExpiresAt.Before(now) {
		return TaskLease{}, fmt.Errorf("task lease expired")
	}

	current.LastHeartbeatAt = now
	current.ExpiresAt = now.Add(ttl)
	requestLeases[lease.TaskID] = current
	return current, nil
}

func (s *MemoryStore) GetTaskLease(_ context.Context, requestID, taskID string) (TaskLease, bool, error) {
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	requestLeases := s.leases[requestID]
	if requestLeases == nil {
		return TaskLease{}, false, nil
	}
	lease, ok := requestLeases[taskID]
	if !ok {
		return TaskLease{}, false, nil
	}
	if lease.ExpiresAt.Before(now) {
		delete(requestLeases, taskID)
		return TaskLease{}, false, nil
	}
	return lease, true, nil
}

func (s *MemoryStore) ReleaseTaskLease(_ context.Context, lease TaskLease) error {
	s.mu.Lock()
	defer s.mu.Unlock()

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
}

func cloneNode(node Node) Node {
	node.Labels = cloneStringMap(node.Labels)
	node.ProfileDigests = cloneStringMap(node.ProfileDigests)
	return node
}

func cloneAttestedNodeRecord(record AttestedNodeRecord) AttestedNodeRecord {
	record.Claims = cloneStringMap(record.Claims)
	record.Measurements = cloneStringMap(record.Measurements)
	return record
}

func cloneStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	out := make(map[string]string, len(input))
	for k, v := range input {
		out[k] = v
	}
	return out
}
