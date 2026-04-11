package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

const defaultEtcdDialTimeout = 5 * time.Second

type EtcdStore struct {
	client *clientv3.Client
	fabric string
	root   string
}

func NewEtcdStore(fabric string, opts EtcdOptions) (*EtcdStore, error) {
	if strings.TrimSpace(fabric) == "" {
		return nil, fmt.Errorf("fabric is required")
	}
	if len(opts.Endpoints) == 0 {
		return nil, fmt.Errorf("etcd endpoints are required")
	}
	dialTimeout := opts.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = defaultEtcdDialTimeout
	}

	client, err := clientv3.New(clientv3.Config{
		Endpoints:   append([]string{}, opts.Endpoints...),
		DialTimeout: dialTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("create etcd client: %w", err)
	}

	return &EtcdStore{
		client: client,
		fabric: fabric,
		root:   path.Join("/agentfab/fabrics", fabric),
	}, nil
}

func (s *EtcdStore) Close() error {
	if s.client == nil {
		return nil
	}
	return s.client.Close()
}

func (s *EtcdStore) UpsertAttestedNode(ctx context.Context, record AttestedNodeRecord) error {
	if record.NodeID == "" {
		return fmt.Errorf("attested node ID is required")
	}
	return s.putJSON(ctx, s.attestedNodeKey(record.NodeID), cloneAttestedNodeRecord(record))
}

func (s *EtcdStore) GetAttestedNode(ctx context.Context, nodeID string) (AttestedNodeRecord, bool, error) {
	record, ok, err := getJSONValue[AttestedNodeRecord](ctx, s.client, s.attestedNodeKey(nodeID))
	if err != nil || !ok {
		return AttestedNodeRecord{}, ok, err
	}
	return cloneAttestedNodeRecord(record), true, nil
}

func (s *EtcdStore) AcquireLeader(_ context.Context, candidateID, candidateAddress string, ttl time.Duration) (LeaderLease, bool, error) {
	if candidateID == "" {
		return LeaderLease{}, false, fmt.Errorf("candidate ID is required")
	}
	if ttl <= 0 {
		return LeaderLease{}, false, fmt.Errorf("leader TTL must be positive")
	}

	now := time.Now().UTC()
	var lease LeaderLease
	acquired := false
	_, err := concurrency.NewSTM(s.client, func(stm concurrency.STM) error {
		current, ok, err := s.leaderFromSTM(stm)
		if err != nil {
			return err
		}
		if ok && current.ExpiresAt.After(now) && current.HolderID != candidateID {
			lease = current
			return nil
		}

		epoch, err := s.int64FromSTM(stm, s.leaderEpochKey())
		if err != nil {
			return err
		}
		epoch++
		lease = LeaderLease{
			Fabric:          s.fabric,
			HolderID:        candidateID,
			HolderAddress:   candidateAddress,
			Epoch:           epoch,
			AcquiredAt:      now,
			LastHeartbeatAt: now,
			ExpiresAt:       now.Add(ttl),
		}
		if err := putJSONSTM(stm, s.leaderKey(), lease); err != nil {
			return err
		}
		stm.Put(s.leaderEpochKey(), strconv.FormatInt(epoch, 10))
		acquired = true
		return nil
	})
	return lease, acquired, err
}

func (s *EtcdStore) RenewLeader(_ context.Context, lease LeaderLease, ttl time.Duration) (LeaderLease, error) {
	if ttl <= 0 {
		return LeaderLease{}, fmt.Errorf("leader TTL must be positive")
	}

	now := time.Now().UTC()
	var renewed LeaderLease
	_, err := concurrency.NewSTM(s.client, func(stm concurrency.STM) error {
		current, ok, err := s.leaderFromSTM(stm)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no leader lease")
		}
		if current.HolderID != lease.HolderID || current.Epoch != lease.Epoch {
			return fmt.Errorf("leader lease is no longer owned by %q", lease.HolderID)
		}
		if current.ExpiresAt.Before(now) {
			return fmt.Errorf("leader lease expired")
		}
		current.LastHeartbeatAt = now
		current.ExpiresAt = now.Add(ttl)
		if err := putJSONSTM(stm, s.leaderKey(), current); err != nil {
			return err
		}
		renewed = current
		return nil
	})
	return renewed, err
}

func (s *EtcdStore) GetLeader(ctx context.Context) (LeaderLease, bool, error) {
	lease, ok, err := getJSONValue[LeaderLease](ctx, s.client, s.leaderKey())
	if err != nil || !ok {
		return LeaderLease{}, ok, err
	}
	if lease.ExpiresAt.Before(time.Now().UTC()) {
		return LeaderLease{}, false, nil
	}
	return lease, true, nil
}

func (s *EtcdStore) ReleaseLeader(_ context.Context, lease LeaderLease) error {
	_, err := concurrency.NewSTM(s.client, func(stm concurrency.STM) error {
		current, ok, err := s.leaderFromSTM(stm)
		if err != nil || !ok {
			return err
		}
		if current.HolderID != lease.HolderID || current.Epoch != lease.Epoch {
			return nil
		}
		stm.Del(s.leaderKey())
		return nil
	})
	return err
}

func (s *EtcdStore) RegisterNode(ctx context.Context, node Node) error {
	if node.ID == "" {
		return fmt.Errorf("node ID is required")
	}
	if node.State == "" {
		node.State = NodeStateReady
	}
	if node.StartedAt.IsZero() {
		node.StartedAt = time.Now().UTC()
	}
	if node.LastHeartbeatAt.IsZero() {
		node.LastHeartbeatAt = node.StartedAt
	}
	return s.putJSON(ctx, s.nodeKey(node.ID), cloneNode(node))
}

func (s *EtcdStore) HeartbeatNode(_ context.Context, nodeID string, at time.Time) error {
	if nodeID == "" {
		return fmt.Errorf("node ID is required")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}

	_, err := concurrency.NewSTM(s.client, func(stm concurrency.STM) error {
		node, ok, err := nodeFromSTM(stm, s.nodeKey(nodeID))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("node %q not found", nodeID)
		}
		node.LastHeartbeatAt = at
		if node.State == "" || node.State == NodeStateUnknown || node.State == NodeStateUnavailable {
			node.State = NodeStateReady
		}
		return putJSONSTM(stm, s.nodeKey(nodeID), cloneNode(node))
	})
	return err
}

func (s *EtcdStore) GetNode(ctx context.Context, nodeID string) (Node, bool, error) {
	node, ok, err := getJSONValue[Node](ctx, s.client, s.nodeKey(nodeID))
	if err != nil || !ok {
		return Node{}, ok, err
	}
	normalizeMembershipLiveness(map[string]Node{node.ID: node}, map[string]AgentInstance{}, time.Now().UTC())
	node = normalizeSingleNode(node)
	return node, true, nil
}

func (s *EtcdStore) ListNodes(ctx context.Context) ([]Node, error) {
	nodes, err := s.listNodes(ctx)
	if err != nil {
		return nil, err
	}
	instances, err := s.listInstances(ctx, InstanceFilter{})
	if err != nil {
		return nil, err
	}
	nodeMap := make(map[string]Node, len(nodes))
	for _, node := range nodes {
		nodeMap[node.ID] = node
	}
	instanceMap := make(map[string]AgentInstance, len(instances))
	for _, instance := range instances {
		instanceMap[instance.ID] = instance
	}
	normalizeMembershipLiveness(nodeMap, instanceMap, time.Now().UTC())

	result := make([]Node, 0, len(nodeMap))
	for _, node := range nodeMap {
		result = append(result, cloneNode(node))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (s *EtcdStore) RegisterInstance(ctx context.Context, instance AgentInstance) error {
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
		instance.StartedAt = time.Now().UTC()
	}
	if instance.LastHeartbeatAt.IsZero() {
		instance.LastHeartbeatAt = instance.StartedAt
	}
	return s.putJSON(ctx, s.instanceKey(instance.ID), instance)
}

func (s *EtcdStore) HeartbeatInstance(_ context.Context, instanceID string, at time.Time) error {
	if instanceID == "" {
		return fmt.Errorf("instance ID is required")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}

	_, err := concurrency.NewSTM(s.client, func(stm concurrency.STM) error {
		instance, ok, err := instanceFromSTM(stm, s.instanceKey(instanceID))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("instance %q not found", instanceID)
		}
		instance.LastHeartbeatAt = at
		if instance.State == "" || instance.State == InstanceStateUnknown || instance.State == InstanceStateUnavailable {
			instance.State = InstanceStateReady
		}
		return putJSONSTM(stm, s.instanceKey(instanceID), instance)
	})
	return err
}

func (s *EtcdStore) GetInstance(ctx context.Context, instanceID string) (AgentInstance, bool, error) {
	instance, ok, err := getJSONValue[AgentInstance](ctx, s.client, s.instanceKey(instanceID))
	if err != nil || !ok {
		return AgentInstance{}, ok, err
	}
	node, nodeOK, nodeErr := s.GetNode(ctx, instance.NodeID)
	if nodeErr != nil {
		return AgentInstance{}, false, nodeErr
	}
	current := instance
	if instanceHeartbeatExpired(current, node, nodeOK, time.Now().UTC()) {
		current.State = InstanceStateUnavailable
	}
	return current, true, nil
}

func (s *EtcdStore) ListInstances(ctx context.Context, filter InstanceFilter) ([]AgentInstance, error) {
	instances, err := s.listInstances(ctx, filter)
	if err != nil {
		return nil, err
	}
	nodes, err := s.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	nodeMap := make(map[string]Node, len(nodes))
	for _, node := range nodes {
		nodeMap[node.ID] = node
	}

	now := time.Now().UTC()
	result := make([]AgentInstance, 0, len(instances))
	for _, instance := range instances {
		node, ok := nodeMap[instance.NodeID]
		if instanceHeartbeatExpired(instance, node, ok, now) {
			instance.State = InstanceStateUnavailable
		}
		if filter.State != "" && instance.State != filter.State {
			continue
		}
		result = append(result, instance)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (s *EtcdStore) RemoveInstance(ctx context.Context, instanceID string) error {
	_, err := s.client.Delete(ctx, s.instanceKey(instanceID))
	return err
}

func (s *EtcdStore) UpsertRequest(ctx context.Context, request RequestRecord) error {
	if request.ID == "" {
		return fmt.Errorf("request ID is required")
	}
	now := time.Now().UTC()
	current, ok, err := s.GetRequest(ctx, request.ID)
	if err != nil {
		return err
	}
	if request.CreatedAt.IsZero() {
		if ok {
			request.CreatedAt = current.CreatedAt
		} else {
			request.CreatedAt = now
		}
	}
	request.UpdatedAt = now
	return s.putJSON(ctx, s.requestKey(request.ID), request)
}

func (s *EtcdStore) GetRequest(ctx context.Context, requestID string) (RequestRecord, bool, error) {
	value, ok, err := getJSONValue[RequestRecord](ctx, s.client, s.requestKey(requestID))
	if err != nil || !ok {
		return RequestRecord{}, ok, err
	}
	return value, true, nil
}

func (s *EtcdStore) ListRequests(ctx context.Context) ([]RequestRecord, error) {
	response, err := s.client.Get(ctx, s.requestPrefix(), clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	requests := make([]RequestRecord, 0, len(response.Kvs))
	for _, kv := range response.Kvs {
		var request RequestRecord
		if err := json.Unmarshal(kv.Value, &request); err != nil {
			return nil, fmt.Errorf("decode request %q: %w", string(kv.Key), err)
		}
		requests = append(requests, request)
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].ID < requests[j].ID })
	return requests, nil
}

func (s *EtcdStore) UpsertTask(ctx context.Context, task TaskRecord) error {
	if task.RequestID == "" {
		return fmt.Errorf("task request ID is required")
	}
	if task.TaskID == "" {
		return fmt.Errorf("task ID is required")
	}
	task.UpdatedAt = time.Now().UTC()
	return s.putJSON(ctx, s.taskKey(task.RequestID, task.TaskID), task)
}

func (s *EtcdStore) ListTasks(ctx context.Context, requestID string) ([]TaskRecord, error) {
	response, err := s.client.Get(ctx, s.taskPrefix(requestID), clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	tasks := make([]TaskRecord, 0, len(response.Kvs))
	for _, kv := range response.Kvs {
		var task TaskRecord
		if err := json.Unmarshal(kv.Value, &task); err != nil {
			return nil, fmt.Errorf("decode task %q: %w", string(kv.Key), err)
		}
		tasks = append(tasks, task)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].TaskID < tasks[j].TaskID })
	return tasks, nil
}

func (s *EtcdStore) AcquireTaskLease(_ context.Context, lease TaskLease, ttl time.Duration) (TaskLease, bool, error) {
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

	now := time.Now().UTC()
	acquired := false
	_, err := concurrency.NewSTM(s.client, func(stm concurrency.STM) error {
		current, ok, err := taskLeaseFromSTM(stm, s.taskLeaseKey(lease.RequestID, lease.TaskID))
		if err != nil {
			return err
		}
		if ok && current.ExpiresAt.After(now) && current.OwnerID != lease.OwnerID {
			lease = current
			return nil
		}

		epoch, err := s.int64FromSTM(stm, s.taskLeaseEpochKey(lease.RequestID, lease.TaskID))
		if err != nil {
			return err
		}
		epoch++
		lease.Epoch = epoch
		lease.AcquiredAt = now
		lease.LastHeartbeatAt = now
		lease.ExpiresAt = now.Add(ttl)
		if err := putJSONSTM(stm, s.taskLeaseKey(lease.RequestID, lease.TaskID), lease); err != nil {
			return err
		}
		stm.Put(s.taskLeaseEpochKey(lease.RequestID, lease.TaskID), strconv.FormatInt(epoch, 10))
		acquired = true
		return nil
	})
	return lease, acquired, err
}

func (s *EtcdStore) AcquireProfileLease(_ context.Context, lease ProfileLease, ttl time.Duration) (ProfileLease, bool, error) {
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

	now := time.Now().UTC()
	acquired := false
	_, err := concurrency.NewSTM(s.client, func(stm concurrency.STM) error {
		current, ok, err := profileLeaseFromSTM(stm, s.profileLeaseKey(lease.Profile))
		if err != nil {
			return err
		}
		if ok && current.ExpiresAt.After(now) {
			if current.OwnerID == lease.OwnerID && current.AssignedInstance == lease.AssignedInstance {
				current.LastHeartbeatAt = now
				current.ExpiresAt = now.Add(ttl)
				if err := putJSONSTM(stm, s.profileLeaseKey(lease.Profile), current); err != nil {
					return err
				}
				lease = current
				acquired = true
				return nil
			}
			lease = current
			return nil
		}

		epoch, err := s.int64FromSTM(stm, s.profileLeaseEpochKey(lease.Profile))
		if err != nil {
			return err
		}
		epoch++
		lease.Epoch = epoch
		lease.AcquiredAt = now
		lease.LastHeartbeatAt = now
		lease.ExpiresAt = now.Add(ttl)
		if err := putJSONSTM(stm, s.profileLeaseKey(lease.Profile), lease); err != nil {
			return err
		}
		stm.Put(s.profileLeaseEpochKey(lease.Profile), strconv.FormatInt(epoch, 10))
		acquired = true
		return nil
	})
	return lease, acquired, err
}

func (s *EtcdStore) RenewProfileLease(_ context.Context, lease ProfileLease, ttl time.Duration) (ProfileLease, error) {
	if ttl <= 0 {
		return ProfileLease{}, fmt.Errorf("profile lease TTL must be positive")
	}

	now := time.Now().UTC()
	var renewed ProfileLease
	_, err := concurrency.NewSTM(s.client, func(stm concurrency.STM) error {
		current, ok, err := profileLeaseFromSTM(stm, s.profileLeaseKey(lease.Profile))
		if err != nil {
			return err
		}
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
		if err := putJSONSTM(stm, s.profileLeaseKey(lease.Profile), current); err != nil {
			return err
		}
		renewed = current
		return nil
	})
	return renewed, err
}

func (s *EtcdStore) GetProfileLease(ctx context.Context, profile string) (ProfileLease, bool, error) {
	lease, ok, err := getJSONValue[ProfileLease](ctx, s.client, s.profileLeaseKey(profile))
	if err != nil || !ok {
		return ProfileLease{}, ok, err
	}
	if lease.ExpiresAt.Before(time.Now().UTC()) {
		return ProfileLease{}, false, nil
	}
	return lease, true, nil
}

func (s *EtcdStore) ReleaseProfileLease(ctx context.Context, lease ProfileLease) error {
	_, err := concurrency.NewSTM(s.client, func(stm concurrency.STM) error {
		current, ok, err := profileLeaseFromSTM(stm, s.profileLeaseKey(lease.Profile))
		if err != nil || !ok {
			return err
		}
		if current.OwnerID != lease.OwnerID || current.Epoch != lease.Epoch || current.AssignedInstance != lease.AssignedInstance {
			return nil
		}
		stm.Del(s.profileLeaseKey(lease.Profile))
		return nil
	})
	return err
}

func (s *EtcdStore) RenewTaskLease(_ context.Context, lease TaskLease, ttl time.Duration) (TaskLease, error) {
	if ttl <= 0 {
		return TaskLease{}, fmt.Errorf("task lease TTL must be positive")
	}

	now := time.Now().UTC()
	var renewed TaskLease
	_, err := concurrency.NewSTM(s.client, func(stm concurrency.STM) error {
		current, ok, err := taskLeaseFromSTM(stm, s.taskLeaseKey(lease.RequestID, lease.TaskID))
		if err != nil {
			return err
		}
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
		if err := putJSONSTM(stm, s.taskLeaseKey(lease.RequestID, lease.TaskID), current); err != nil {
			return err
		}
		renewed = current
		return nil
	})
	return renewed, err
}

func (s *EtcdStore) GetTaskLease(ctx context.Context, requestID, taskID string) (TaskLease, bool, error) {
	lease, ok, err := getJSONValue[TaskLease](ctx, s.client, s.taskLeaseKey(requestID, taskID))
	if err != nil || !ok {
		return TaskLease{}, ok, err
	}
	if lease.ExpiresAt.Before(time.Now().UTC()) {
		return TaskLease{}, false, nil
	}
	return lease, true, nil
}

func (s *EtcdStore) ReleaseTaskLease(ctx context.Context, lease TaskLease) error {
	_, err := concurrency.NewSTM(s.client, func(stm concurrency.STM) error {
		current, ok, err := taskLeaseFromSTM(stm, s.taskLeaseKey(lease.RequestID, lease.TaskID))
		if err != nil || !ok {
			return err
		}
		if current.OwnerID != lease.OwnerID || current.Epoch != lease.Epoch {
			return nil
		}
		stm.Del(s.taskLeaseKey(lease.RequestID, lease.TaskID))
		return nil
	})
	return err
}

func (s *EtcdStore) leaderFromSTM(stm concurrency.STM) (LeaderLease, bool, error) {
	value := stm.Get(s.leaderKey())
	if value == "" {
		return LeaderLease{}, false, nil
	}
	var lease LeaderLease
	if err := json.Unmarshal([]byte(value), &lease); err != nil {
		return LeaderLease{}, false, fmt.Errorf("decode leader lease: %w", err)
	}
	return lease, true, nil
}

func nodeFromSTM(stm concurrency.STM, key string) (Node, bool, error) {
	value := stm.Get(key)
	if value == "" {
		return Node{}, false, nil
	}
	var node Node
	if err := json.Unmarshal([]byte(value), &node); err != nil {
		return Node{}, false, fmt.Errorf("decode node: %w", err)
	}
	return node, true, nil
}

func instanceFromSTM(stm concurrency.STM, key string) (AgentInstance, bool, error) {
	value := stm.Get(key)
	if value == "" {
		return AgentInstance{}, false, nil
	}
	var instance AgentInstance
	if err := json.Unmarshal([]byte(value), &instance); err != nil {
		return AgentInstance{}, false, fmt.Errorf("decode instance: %w", err)
	}
	return instance, true, nil
}

func taskLeaseFromSTM(stm concurrency.STM, key string) (TaskLease, bool, error) {
	value := stm.Get(key)
	if value == "" {
		return TaskLease{}, false, nil
	}
	var lease TaskLease
	if err := json.Unmarshal([]byte(value), &lease); err != nil {
		return TaskLease{}, false, fmt.Errorf("decode task lease: %w", err)
	}
	return lease, true, nil
}

func profileLeaseFromSTM(stm concurrency.STM, key string) (ProfileLease, bool, error) {
	value := stm.Get(key)
	if value == "" {
		return ProfileLease{}, false, nil
	}
	var lease ProfileLease
	if err := json.Unmarshal([]byte(value), &lease); err != nil {
		return ProfileLease{}, false, fmt.Errorf("decode profile lease: %w", err)
	}
	return lease, true, nil
}

func (s *EtcdStore) int64FromSTM(stm concurrency.STM, key string) (int64, error) {
	value := stm.Get(key)
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse integer key %q: %w", key, err)
	}
	return parsed, nil
}

func putJSONSTM(stm concurrency.STM, key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	stm.Put(key, string(data))
	return nil
}

func getJSONValue[T any](ctx context.Context, client *clientv3.Client, key string) (T, bool, error) {
	var zero T

	response, err := client.Get(ctx, key)
	if err != nil {
		return zero, false, err
	}
	if len(response.Kvs) == 0 {
		return zero, false, nil
	}
	var value T
	if err := json.Unmarshal(response.Kvs[0].Value, &value); err != nil {
		return zero, false, fmt.Errorf("decode %q: %w", key, err)
	}
	return value, true, nil
}

func (s *EtcdStore) putJSON(ctx context.Context, key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.client.Put(ctx, key, string(data))
	return err
}

func (s *EtcdStore) listNodes(ctx context.Context) ([]Node, error) {
	response, err := s.client.Get(ctx, s.nodePrefix(), clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	nodes := make([]Node, 0, len(response.Kvs))
	for _, kv := range response.Kvs {
		var node Node
		if err := json.Unmarshal(kv.Value, &node); err != nil {
			return nil, fmt.Errorf("decode node %q: %w", string(kv.Key), err)
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}

func (s *EtcdStore) listInstances(ctx context.Context, filter InstanceFilter) ([]AgentInstance, error) {
	response, err := s.client.Get(ctx, s.instancePrefix(), clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	instances := make([]AgentInstance, 0, len(response.Kvs))
	for _, kv := range response.Kvs {
		var instance AgentInstance
		if err := json.Unmarshal(kv.Value, &instance); err != nil {
			return nil, fmt.Errorf("decode instance %q: %w", string(kv.Key), err)
		}
		if filter.Profile != "" && instance.Profile != filter.Profile {
			continue
		}
		if filter.NodeID != "" && instance.NodeID != filter.NodeID {
			continue
		}
		instances = append(instances, instance)
	}
	return instances, nil
}

func normalizeSingleNode(node Node) Node {
	if nodeHeartbeatExpired(node, time.Now().UTC()) {
		node.State = NodeStateUnavailable
	}
	return node
}

func (s *EtcdStore) key(parts ...string) string {
	segments := []string{s.root}
	segments = append(segments, parts...)
	return path.Join(segments...)
}

func (s *EtcdStore) attestedNodePrefix() string { return s.key("attested-nodes") + "/" }
func (s *EtcdStore) attestedNodeKey(nodeID string) string {
	return s.key("attested-nodes", nodeID)
}
func (s *EtcdStore) leaderKey() string            { return s.key("leader", "current") }
func (s *EtcdStore) leaderEpochKey() string       { return s.key("leader", "epoch") }
func (s *EtcdStore) nodePrefix() string           { return s.key("nodes") + "/" }
func (s *EtcdStore) nodeKey(nodeID string) string { return s.key("nodes", nodeID) }
func (s *EtcdStore) instancePrefix() string       { return s.key("instances") + "/" }
func (s *EtcdStore) instanceKey(id string) string { return s.key("instances", id) }
func (s *EtcdStore) requestPrefix() string        { return s.key("requests") + "/" }
func (s *EtcdStore) requestKey(id string) string  { return s.key("requests", id) }
func (s *EtcdStore) taskPrefix(requestID string) string {
	return s.key("tasks", requestID) + "/"
}
func (s *EtcdStore) taskKey(requestID, taskID string) string {
	return s.key("tasks", requestID, taskID)
}
func (s *EtcdStore) taskLeaseKey(requestID, taskID string) string {
	return s.key("task_leases", requestID, taskID)
}
func (s *EtcdStore) taskLeaseEpochKey(requestID, taskID string) string {
	return s.key("task_lease_epochs", requestID, taskID)
}
func (s *EtcdStore) profileLeaseKey(profile string) string {
	return s.key("profile_leases", profile)
}
func (s *EtcdStore) profileLeaseEpochKey(profile string) string {
	return s.key("profile_lease_epochs", profile)
}
