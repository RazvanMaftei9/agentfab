package controlplane

import (
	"context"
	"crypto/tls"
	"fmt"
	"sort"
	"sync"
	"time"

	pb "github.com/razvanmaftei/agentfab/gen/agentfab/v1"
	"github.com/razvanmaftei/agentfab/internal/identity"
	"github.com/razvanmaftei/agentfab/internal/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type RemoteClient struct {
	address   string
	clientTLS *tls.Config

	mu        sync.RWMutex
	overrides map[string]runtime.Endpoint
}

func NewRemoteClient(address string, clientTLS *tls.Config) *RemoteClient {
	return &RemoteClient{
		address:   address,
		clientTLS: clientTLS,
		overrides: make(map[string]runtime.Endpoint),
	}
}

func (c *RemoteClient) Register(_ context.Context, name string, endpoint runtime.Endpoint) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.overrides[name] = endpoint
	return nil
}

func (c *RemoteClient) AttestNode(ctx context.Context, request identity.NodeAttestation) (identity.AttestedNode, error) {
	client, conn, err := c.client(ctx)
	if err != nil {
		return identity.AttestedNode{}, err
	}
	defer conn.Close()

	response, err := client.AttestNode(ctx, &pb.AttestNodeRequest{
		Type:         request.Type,
		Claims:       cloneLabels(request.Claims),
		Measurements: cloneLabels(request.Measurements),
		Token:        request.Token,
	})
	if err != nil {
		return identity.AttestedNode{}, err
	}

	return identity.AttestedNode{
		NodeID:       response.GetNodeId(),
		TrustDomain:  response.GetTrustDomain(),
		Claims:       cloneLabels(response.GetClaims()),
		Measurements: cloneLabels(response.GetMeasurements()),
		AttestedAt:   time.Unix(0, response.GetAttestedAtUnixNano()).UTC(),
		ExpiresAt:    time.Unix(0, response.GetExpiresAtUnixNano()).UTC(),
	}, nil
}

func (c *RemoteClient) Resolve(ctx context.Context, name string) (runtime.Endpoint, error) {
	c.mu.RLock()
	if endpoint, ok := c.overrides[name]; ok {
		c.mu.RUnlock()
		return endpoint, nil
	}
	c.mu.RUnlock()

	client, conn, err := c.client(ctx)
	if err != nil {
		return runtime.Endpoint{}, err
	}
	defer conn.Close()

	response, err := client.ResolveEndpoint(ctx, &pb.ResolveEndpointRequest{Name: name})
	if err != nil {
		return runtime.Endpoint{}, err
	}
	if !response.GetFound() || response.GetAddress() == "" {
		return runtime.Endpoint{}, fmt.Errorf("agent %q not found in control plane discovery", name)
	}
	return runtime.Endpoint{
		Address: response.GetAddress(),
		Local:   response.GetLocal(),
	}, nil
}

func (c *RemoteClient) List(ctx context.Context) ([]string, error) {
	c.mu.RLock()
	names := make([]string, 0, len(c.overrides)+1)
	seen := make(map[string]bool, len(c.overrides)+1)
	for name := range c.overrides {
		names = append(names, name)
		seen[name] = true
	}
	c.mu.RUnlock()

	if c.address != "" && !seen["conductor"] {
		names = append(names, "conductor")
		seen["conductor"] = true
	}

	client, conn, err := c.client(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	response, err := client.ListParticipants(ctx, &pb.ListParticipantsRequest{})
	if err != nil {
		return nil, err
	}
	for _, name := range response.GetNames() {
		if seen[name] {
			continue
		}
		names = append(names, name)
		seen[name] = true
	}

	sort.Strings(names)
	return names, nil
}

func (c *RemoteClient) UpsertRequest(ctx context.Context, request RequestRecord) error {
	client, conn, err := c.client(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	_, err = client.UpsertRequest(ctx, &pb.UpsertRequestRequest{
		Request: requestRecordToProto(request),
	})
	return err
}

func (c *RemoteClient) Deregister(_ context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.overrides, name)
	return nil
}

func (c *RemoteClient) RegisterNode(ctx context.Context, node Node) error {
	client, conn, err := c.client(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	_, err = client.RegisterNode(ctx, &pb.RegisterNodeRequest{
		NodeId:                  node.ID,
		Address:                 node.Address,
		State:                   string(node.State),
		Labels:                  cloneLabels(node.Labels),
		BundleDigest:            node.BundleDigest,
		ProfileDigests:          cloneLabels(node.ProfileDigests),
		MaxInstances:            int32(node.Capacity.MaxInstances),
		MaxTasks:                int32(node.Capacity.MaxTasks),
		StartedAtUnixNano:       node.StartedAt.UnixNano(),
		LastHeartbeatAtUnixNano: node.LastHeartbeatAt.UnixNano(),
	})
	return err
}

func (c *RemoteClient) HeartbeatNode(ctx context.Context, nodeID string, at time.Time) error {
	client, conn, err := c.client(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	_, err = client.HeartbeatNode(ctx, &pb.HeartbeatNodeRequest{
		NodeId:     nodeID,
		AtUnixNano: at.UnixNano(),
	})
	return err
}

func (c *RemoteClient) GetNode(ctx context.Context, nodeID string) (Node, bool, error) {
	client, conn, err := c.client(ctx)
	if err != nil {
		return Node{}, false, err
	}
	defer conn.Close()

	response, err := client.GetNode(ctx, &pb.GetNodeRequest{NodeId: nodeID})
	if err != nil {
		return Node{}, false, err
	}
	if !response.GetFound() {
		return Node{}, false, nil
	}
	return nodeFromProto(response.GetNode()), true, nil
}

func (c *RemoteClient) ListNodes(ctx context.Context) ([]Node, error) {
	client, conn, err := c.client(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	response, err := client.ListNodes(ctx, &pb.ListNodesRequest{})
	if err != nil {
		return nil, err
	}
	nodes := make([]Node, 0, len(response.GetNodes()))
	for _, node := range response.GetNodes() {
		nodes = append(nodes, nodeFromProto(node))
	}
	return nodes, nil
}

func (c *RemoteClient) RegisterInstance(ctx context.Context, instance AgentInstance) error {
	client, conn, err := c.client(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	_, err = client.RegisterInstance(ctx, &pb.RegisterInstanceRequest{
		InstanceId:              instance.ID,
		Profile:                 instance.Profile,
		NodeId:                  instance.NodeID,
		BundleDigest:            instance.BundleDigest,
		ProfileDigest:           instance.ProfileDigest,
		EndpointAddress:         instance.Endpoint.Address,
		EndpointLocal:           instance.Endpoint.Local,
		State:                   string(instance.State),
		StartedAtUnixNano:       instance.StartedAt.UnixNano(),
		LastHeartbeatAtUnixNano: instance.LastHeartbeatAt.UnixNano(),
	})
	return err
}

func (c *RemoteClient) HeartbeatInstance(ctx context.Context, instanceID string, at time.Time) error {
	client, conn, err := c.client(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	_, err = client.HeartbeatInstance(ctx, &pb.HeartbeatInstanceRequest{
		InstanceId: instanceID,
		AtUnixNano: at.UnixNano(),
	})
	return err
}

func (c *RemoteClient) GetInstance(ctx context.Context, instanceID string) (AgentInstance, bool, error) {
	client, conn, err := c.client(ctx)
	if err != nil {
		return AgentInstance{}, false, err
	}
	defer conn.Close()

	response, err := client.GetInstance(ctx, &pb.GetInstanceRequest{InstanceId: instanceID})
	if err != nil {
		return AgentInstance{}, false, err
	}
	if !response.GetFound() {
		return AgentInstance{}, false, nil
	}
	return instanceFromProto(response.GetInstance()), true, nil
}

func (c *RemoteClient) ListInstances(ctx context.Context, filter InstanceFilter) ([]AgentInstance, error) {
	client, conn, err := c.client(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	response, err := client.ListInstances(ctx, &pb.ListInstancesRequest{
		Profile: filter.Profile,
		NodeId:  filter.NodeID,
		State:   string(filter.State),
	})
	if err != nil {
		return nil, err
	}
	instances := make([]AgentInstance, 0, len(response.GetInstances()))
	for _, instance := range response.GetInstances() {
		instances = append(instances, instanceFromProto(instance))
	}
	return instances, nil
}

func (c *RemoteClient) RemoveInstance(ctx context.Context, instanceID string) error {
	client, conn, err := c.client(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	_, err = client.RemoveInstance(ctx, &pb.RemoveInstanceRequest{InstanceId: instanceID})
	return err
}

func (c *RemoteClient) AcquireLeader(ctx context.Context, candidateID, candidateAddress string, ttl time.Duration) (LeaderLease, bool, error) {
	client, conn, err := c.client(ctx)
	if err != nil {
		return LeaderLease{}, false, err
	}
	defer conn.Close()

	response, err := client.AcquireLeader(ctx, &pb.AcquireLeaderRequest{
		CandidateId:      candidateID,
		CandidateAddress: candidateAddress,
		TtlNanos:         ttl.Nanoseconds(),
	})
	if err != nil {
		return LeaderLease{}, false, err
	}
	return leaderLeaseFromProto(response.GetLeader()), response.GetAcquired(), nil
}

func (c *RemoteClient) RenewLeader(ctx context.Context, lease LeaderLease, ttl time.Duration) (LeaderLease, error) {
	client, conn, err := c.client(ctx)
	if err != nil {
		return LeaderLease{}, err
	}
	defer conn.Close()

	response, err := client.RenewLeader(ctx, &pb.RenewLeaderRequest{
		Lease:    leaderLeaseToProto(lease),
		TtlNanos: ttl.Nanoseconds(),
	})
	if err != nil {
		return LeaderLease{}, err
	}
	return leaderLeaseFromProto(response.GetLease()), nil
}

func (c *RemoteClient) ReleaseLeader(ctx context.Context, lease LeaderLease) error {
	client, conn, err := c.client(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	_, err = client.ReleaseLeader(ctx, &pb.ReleaseLeaderRequest{
		Lease: leaderLeaseToProto(lease),
	})
	return err
}

func (c *RemoteClient) GetLeader(ctx context.Context) (LeaderLease, bool, error) {
	client, conn, err := c.client(ctx)
	if err != nil {
		return LeaderLease{}, false, err
	}
	defer conn.Close()

	response, err := client.GetLeader(ctx, &pb.GetLeaderRequest{})
	if err != nil {
		return LeaderLease{}, false, err
	}
	if !response.GetFound() || response.GetLeader() == nil {
		return LeaderLease{}, false, nil
	}
	return leaderLeaseFromProto(response.GetLeader()), true, nil
}

func (c *RemoteClient) GetRequest(ctx context.Context, requestID string) (RequestRecord, bool, error) {
	client, conn, err := c.client(ctx)
	if err != nil {
		return RequestRecord{}, false, err
	}
	defer conn.Close()

	response, err := client.GetRequest(ctx, &pb.GetRequestRequest{RequestId: requestID})
	if err != nil {
		return RequestRecord{}, false, err
	}
	if !response.GetFound() || response.GetRequest() == nil {
		return RequestRecord{}, false, nil
	}
	return requestRecordFromProto(response.GetRequest()), true, nil
}

func (c *RemoteClient) ListRequests(ctx context.Context) ([]RequestRecord, error) {
	client, conn, err := c.client(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	response, err := client.ListRequests(ctx, &pb.ListRequestsRequest{})
	if err != nil {
		return nil, err
	}
	records := make([]RequestRecord, 0, len(response.GetRequests()))
	for _, record := range response.GetRequests() {
		records = append(records, requestRecordFromProto(record))
	}
	return records, nil
}

func (c *RemoteClient) UpsertTask(ctx context.Context, task TaskRecord) error {
	client, conn, err := c.client(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	_, err = client.UpsertTask(ctx, &pb.UpsertTaskRequest{
		Task: taskRecordToProto(task),
	})
	return err
}

func (c *RemoteClient) ListTasks(ctx context.Context, requestID string) ([]TaskRecord, error) {
	client, conn, err := c.client(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	response, err := client.ListTasks(ctx, &pb.ListTasksRequest{RequestId: requestID})
	if err != nil {
		return nil, err
	}
	records := make([]TaskRecord, 0, len(response.GetTasks()))
	for _, record := range response.GetTasks() {
		records = append(records, taskRecordFromProto(record))
	}
	return records, nil
}

func (c *RemoteClient) AcquireTaskLease(ctx context.Context, lease TaskLease, ttl time.Duration) (TaskLease, bool, error) {
	client, conn, err := c.client(ctx)
	if err != nil {
		return TaskLease{}, false, err
	}
	defer conn.Close()

	response, err := client.AcquireTaskLease(ctx, &pb.AcquireTaskLeaseRequest{
		Lease:    taskLeaseToProto(lease),
		TtlNanos: ttl.Nanoseconds(),
	})
	if err != nil {
		return TaskLease{}, false, err
	}
	return taskLeaseFromProto(response.GetLease()), response.GetAcquired(), nil
}

func (c *RemoteClient) AcquireProfileLease(ctx context.Context, lease ProfileLease, ttl time.Duration) (ProfileLease, bool, error) {
	client, conn, err := c.client(ctx)
	if err != nil {
		return ProfileLease{}, false, err
	}
	defer conn.Close()

	response, err := client.AcquireProfileLease(ctx, &pb.AcquireProfileLeaseRequest{
		Lease:    profileLeaseToProto(lease),
		TtlNanos: ttl.Nanoseconds(),
	})
	if err != nil {
		return ProfileLease{}, false, err
	}
	return profileLeaseFromProto(response.GetLease()), response.GetAcquired(), nil
}

func (c *RemoteClient) RenewProfileLease(ctx context.Context, lease ProfileLease, ttl time.Duration) (ProfileLease, error) {
	client, conn, err := c.client(ctx)
	if err != nil {
		return ProfileLease{}, err
	}
	defer conn.Close()

	response, err := client.RenewProfileLease(ctx, &pb.RenewProfileLeaseRequest{
		Lease:    profileLeaseToProto(lease),
		TtlNanos: ttl.Nanoseconds(),
	})
	if err != nil {
		return ProfileLease{}, err
	}
	return profileLeaseFromProto(response.GetLease()), nil
}

func (c *RemoteClient) GetProfileLease(ctx context.Context, profile string) (ProfileLease, bool, error) {
	client, conn, err := c.client(ctx)
	if err != nil {
		return ProfileLease{}, false, err
	}
	defer conn.Close()

	response, err := client.GetProfileLease(ctx, &pb.GetProfileLeaseRequest{Profile: profile})
	if err != nil {
		return ProfileLease{}, false, err
	}
	if !response.GetFound() || response.GetLease() == nil {
		return ProfileLease{}, false, nil
	}
	return profileLeaseFromProto(response.GetLease()), true, nil
}

func (c *RemoteClient) ReleaseProfileLease(ctx context.Context, lease ProfileLease) error {
	client, conn, err := c.client(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	_, err = client.ReleaseProfileLease(ctx, &pb.ReleaseProfileLeaseRequest{
		Lease: profileLeaseToProto(lease),
	})
	return err
}

func (c *RemoteClient) RenewTaskLease(ctx context.Context, lease TaskLease, ttl time.Duration) (TaskLease, error) {
	client, conn, err := c.client(ctx)
	if err != nil {
		return TaskLease{}, err
	}
	defer conn.Close()

	response, err := client.RenewTaskLease(ctx, &pb.RenewTaskLeaseRequest{
		Lease:    taskLeaseToProto(lease),
		TtlNanos: ttl.Nanoseconds(),
	})
	if err != nil {
		return TaskLease{}, err
	}
	return taskLeaseFromProto(response.GetLease()), nil
}

func (c *RemoteClient) GetTaskLease(ctx context.Context, requestID, taskID string) (TaskLease, bool, error) {
	client, conn, err := c.client(ctx)
	if err != nil {
		return TaskLease{}, false, err
	}
	defer conn.Close()

	response, err := client.GetTaskLease(ctx, &pb.GetTaskLeaseRequest{
		RequestId: requestID,
		TaskId:    taskID,
	})
	if err != nil {
		return TaskLease{}, false, err
	}
	if !response.GetFound() || response.GetLease() == nil {
		return TaskLease{}, false, nil
	}
	return taskLeaseFromProto(response.GetLease()), true, nil
}

func (c *RemoteClient) ReleaseTaskLease(ctx context.Context, lease TaskLease) error {
	client, conn, err := c.client(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	_, err = client.ReleaseTaskLease(ctx, &pb.ReleaseTaskLeaseRequest{
		Lease: taskLeaseToProto(lease),
	})
	return err
}

func (c *RemoteClient) client(ctx context.Context) (pb.ControlPlaneServiceClient, *grpc.ClientConn, error) {
	if c.clientTLS == nil {
		return nil, nil, fmt.Errorf("remote control plane requires client TLS")
	}
	if c.address == "" {
		return nil, nil, fmt.Errorf("control-plane address is not configured")
	}

	conn, err := grpc.NewClient(c.address, grpc.WithTransportCredentials(credentials.NewTLS(c.clientTLS)))
	if err != nil {
		return nil, nil, fmt.Errorf("dial control-plane service at %s: %w", c.address, err)
	}
	return pb.NewControlPlaneServiceClient(conn), conn, nil
}

func nodeFromProto(view *pb.NodeView) Node {
	if view == nil {
		return Node{}
	}
	return Node{
		ID:             view.GetId(),
		Address:        view.GetAddress(),
		State:          NodeState(view.GetState()),
		Labels:         cloneLabels(view.GetLabels()),
		BundleDigest:   view.GetBundleDigest(),
		ProfileDigests: cloneLabels(view.GetProfileDigests()),
		Capacity: NodeCapacity{
			MaxInstances: int(view.GetMaxInstances()),
			MaxTasks:     int(view.GetMaxTasks()),
		},
		StartedAt:       time.Unix(0, view.GetStartedAtUnixNano()).UTC(),
		LastHeartbeatAt: time.Unix(0, view.GetLastHeartbeatAtUnixNano()).UTC(),
	}
}

func instanceFromProto(view *pb.InstanceView) AgentInstance {
	if view == nil {
		return AgentInstance{}
	}
	return AgentInstance{
		ID:            view.GetId(),
		Profile:       view.GetProfile(),
		NodeID:        view.GetNodeId(),
		BundleDigest:  view.GetBundleDigest(),
		ProfileDigest: view.GetProfileDigest(),
		Endpoint: runtime.Endpoint{
			Address: view.GetEndpointAddress(),
			Local:   view.GetEndpointLocal(),
		},
		State:           InstanceState(view.GetState()),
		StartedAt:       time.Unix(0, view.GetStartedAtUnixNano()).UTC(),
		LastHeartbeatAt: time.Unix(0, view.GetLastHeartbeatAtUnixNano()).UTC(),
	}
}

func cloneLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	clone := make(map[string]string, len(labels))
	for key, value := range labels {
		clone[key] = value
	}
	return clone
}

func timeFromUnixNano(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, value).UTC()
}

func leaderLeaseToProto(lease LeaderLease) *pb.LeaderLeaseView {
	return &pb.LeaderLeaseView{
		Fabric:                  lease.Fabric,
		HolderId:                lease.HolderID,
		HolderAddress:           lease.HolderAddress,
		Epoch:                   lease.Epoch,
		AcquiredAtUnixNano:      lease.AcquiredAt.UnixNano(),
		LastHeartbeatAtUnixNano: lease.LastHeartbeatAt.UnixNano(),
		ExpiresAtUnixNano:       lease.ExpiresAt.UnixNano(),
	}
}

func leaderLeaseFromProto(view *pb.LeaderLeaseView) LeaderLease {
	if view == nil {
		return LeaderLease{}
	}
	return LeaderLease{
		Fabric:          view.GetFabric(),
		HolderID:        view.GetHolderId(),
		HolderAddress:   view.GetHolderAddress(),
		Epoch:           view.GetEpoch(),
		AcquiredAt:      timeFromUnixNano(view.GetAcquiredAtUnixNano()),
		LastHeartbeatAt: timeFromUnixNano(view.GetLastHeartbeatAtUnixNano()),
		ExpiresAt:       timeFromUnixNano(view.GetExpiresAtUnixNano()),
	}
}

func requestRecordFromProto(view *pb.RequestRecordView) RequestRecord {
	if view == nil {
		return RequestRecord{}
	}
	return RequestRecord{
		ID:           view.GetId(),
		State:        RequestState(view.GetState()),
		UserRequest:  view.GetUserRequest(),
		GraphVersion: view.GetGraphVersion(),
		LeaderID:     view.GetLeaderId(),
		CreatedAt:    timeFromUnixNano(view.GetCreatedAtUnixNano()),
		UpdatedAt:    timeFromUnixNano(view.GetUpdatedAtUnixNano()),
	}
}

func requestRecordToProto(record RequestRecord) *pb.RequestRecordView {
	return &pb.RequestRecordView{
		Id:                record.ID,
		State:             string(record.State),
		UserRequest:       record.UserRequest,
		GraphVersion:      record.GraphVersion,
		LeaderId:          record.LeaderID,
		CreatedAtUnixNano: record.CreatedAt.UnixNano(),
		UpdatedAtUnixNano: record.UpdatedAt.UnixNano(),
	}
}

func taskRecordFromProto(view *pb.TaskRecordView) TaskRecord {
	if view == nil {
		return TaskRecord{}
	}
	return TaskRecord{
		RequestID:        view.GetRequestId(),
		TaskID:           view.GetTaskId(),
		Profile:          view.GetProfile(),
		AssignedInstance: view.GetAssignedInstance(),
		ExecutionNode:    view.GetExecutionNode(),
		Status:           view.GetStatus(),
		LeaseEpoch:       view.GetLeaseEpoch(),
		UpdatedAt:        timeFromUnixNano(view.GetUpdatedAtUnixNano()),
	}
}

func taskRecordToProto(record TaskRecord) *pb.TaskRecordView {
	return &pb.TaskRecordView{
		RequestId:         record.RequestID,
		TaskId:            record.TaskID,
		Profile:           record.Profile,
		AssignedInstance:  record.AssignedInstance,
		ExecutionNode:     record.ExecutionNode,
		Status:            record.Status,
		LeaseEpoch:        record.LeaseEpoch,
		UpdatedAtUnixNano: record.UpdatedAt.UnixNano(),
	}
}

func taskLeaseFromProto(view *pb.TaskLeaseView) TaskLease {
	if view == nil {
		return TaskLease{}
	}
	return TaskLease{
		RequestID:        view.GetRequestId(),
		TaskID:           view.GetTaskId(),
		Profile:          view.GetProfile(),
		AssignedInstance: view.GetAssignedInstance(),
		ExecutionNode:    view.GetExecutionNode(),
		OwnerID:          view.GetOwnerId(),
		Epoch:            view.GetEpoch(),
		AcquiredAt:       timeFromUnixNano(view.GetAcquiredAtUnixNano()),
		LastHeartbeatAt:  timeFromUnixNano(view.GetLastHeartbeatAtUnixNano()),
		ExpiresAt:        timeFromUnixNano(view.GetExpiresAtUnixNano()),
	}
}

func profileLeaseFromProto(view *pb.ProfileLeaseView) ProfileLease {
	if view == nil {
		return ProfileLease{}
	}
	return ProfileLease{
		Profile:          view.GetProfile(),
		AssignedInstance: view.GetAssignedInstance(),
		ExecutionNode:    view.GetExecutionNode(),
		OwnerID:          view.GetOwnerId(),
		Epoch:            view.GetEpoch(),
		AcquiredAt:       timeFromUnixNano(view.GetAcquiredAtUnixNano()),
		LastHeartbeatAt:  timeFromUnixNano(view.GetLastHeartbeatAtUnixNano()),
		ExpiresAt:        timeFromUnixNano(view.GetExpiresAtUnixNano()),
	}
}

func profileLeaseToProto(lease ProfileLease) *pb.ProfileLeaseView {
	return &pb.ProfileLeaseView{
		Profile:                 lease.Profile,
		AssignedInstance:        lease.AssignedInstance,
		ExecutionNode:           lease.ExecutionNode,
		OwnerId:                 lease.OwnerID,
		Epoch:                   lease.Epoch,
		AcquiredAtUnixNano:      lease.AcquiredAt.UnixNano(),
		LastHeartbeatAtUnixNano: lease.LastHeartbeatAt.UnixNano(),
		ExpiresAtUnixNano:       lease.ExpiresAt.UnixNano(),
	}
}

func taskLeaseToProto(lease TaskLease) *pb.TaskLeaseView {
	return &pb.TaskLeaseView{
		RequestId:               lease.RequestID,
		TaskId:                  lease.TaskID,
		Profile:                 lease.Profile,
		AssignedInstance:        lease.AssignedInstance,
		ExecutionNode:           lease.ExecutionNode,
		OwnerId:                 lease.OwnerID,
		Epoch:                   lease.Epoch,
		AcquiredAtUnixNano:      lease.AcquiredAt.UnixNano(),
		LastHeartbeatAtUnixNano: lease.LastHeartbeatAt.UnixNano(),
		ExpiresAtUnixNano:       lease.ExpiresAt.UnixNano(),
	}
}

var _ runtime.Discovery = (*RemoteClient)(nil)
var _ MembershipWriter = (*RemoteClient)(nil)
var _ Leadership = (*RemoteClient)(nil)
var _ Membership = (*RemoteClient)(nil)
var _ Requests = (*RemoteClient)(nil)
var _ Leases = (*RemoteClient)(nil)
