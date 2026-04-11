package controlplane

import (
	"context"
	"net"
	"testing"
	"time"

	pb "github.com/razvanmaftei/agentfab/gen/agentfab/v1"
	agentgrpc "github.com/razvanmaftei/agentfab/internal/grpc"
	"github.com/razvanmaftei/agentfab/internal/identity"
	"github.com/razvanmaftei/agentfab/internal/runtime"
)

type testControlPlaneService struct {
	store Store
}

func (s *testControlPlaneService) AttestNode(_ context.Context, _ string, request *pb.AttestNodeRequest) (*pb.AttestNodeResponse, error) {
	return &pb.AttestNodeResponse{
		NodeId:       request.GetClaims()["node_id"],
		TrustDomain:  "agentfab.local",
		Claims:       request.GetClaims(),
		Measurements: request.GetMeasurements(),
	}, nil
}

func (s *testControlPlaneService) RegisterNode(ctx context.Context, _ string, request *pb.RegisterNodeRequest) error {
	return s.store.RegisterNode(ctx, Node{
		ID:             request.GetNodeId(),
		Address:        request.GetAddress(),
		State:          NodeState(request.GetState()),
		Labels:         request.GetLabels(),
		BundleDigest:   request.GetBundleDigest(),
		ProfileDigests: request.GetProfileDigests(),
		Capacity: NodeCapacity{
			MaxInstances: int(request.GetMaxInstances()),
			MaxTasks:     int(request.GetMaxTasks()),
		},
		StartedAt:       time.Unix(0, request.GetStartedAtUnixNano()).UTC(),
		LastHeartbeatAt: time.Unix(0, request.GetLastHeartbeatAtUnixNano()).UTC(),
	})
}

func (s *testControlPlaneService) HeartbeatNode(ctx context.Context, _ string, request *pb.HeartbeatNodeRequest) error {
	return s.store.HeartbeatNode(ctx, request.GetNodeId(), time.Unix(0, request.GetAtUnixNano()).UTC())
}

func (s *testControlPlaneService) GetNode(ctx context.Context, _ string, request *pb.GetNodeRequest) (*pb.GetNodeResponse, error) {
	node, ok, err := s.store.GetNode(ctx, request.GetNodeId())
	if err != nil {
		return nil, err
	}
	if !ok {
		return &pb.GetNodeResponse{Found: false}, nil
	}
	return &pb.GetNodeResponse{Found: true, Node: nodeToProto(node)}, nil
}

func (s *testControlPlaneService) ListNodes(ctx context.Context, _ string, _ *pb.ListNodesRequest) (*pb.ListNodesResponse, error) {
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	response := &pb.ListNodesResponse{Nodes: make([]*pb.NodeView, 0, len(nodes))}
	for _, node := range nodes {
		response.Nodes = append(response.Nodes, nodeToProto(node))
	}
	return response, nil
}

func (s *testControlPlaneService) RegisterInstance(ctx context.Context, _ string, request *pb.RegisterInstanceRequest) error {
	return s.store.RegisterInstance(ctx, AgentInstance{
		ID:            request.GetInstanceId(),
		Profile:       request.GetProfile(),
		NodeID:        request.GetNodeId(),
		BundleDigest:  request.GetBundleDigest(),
		ProfileDigest: request.GetProfileDigest(),
		Endpoint: runtime.Endpoint{
			Address: request.GetEndpointAddress(),
			Local:   request.GetEndpointLocal(),
		},
		State:           InstanceState(request.GetState()),
		StartedAt:       time.Unix(0, request.GetStartedAtUnixNano()).UTC(),
		LastHeartbeatAt: time.Unix(0, request.GetLastHeartbeatAtUnixNano()).UTC(),
	})
}

func (s *testControlPlaneService) HeartbeatInstance(ctx context.Context, _ string, request *pb.HeartbeatInstanceRequest) error {
	return s.store.HeartbeatInstance(ctx, request.GetInstanceId(), time.Unix(0, request.GetAtUnixNano()).UTC())
}

func (s *testControlPlaneService) GetInstance(ctx context.Context, _ string, request *pb.GetInstanceRequest) (*pb.GetInstanceResponse, error) {
	instance, ok, err := s.store.GetInstance(ctx, request.GetInstanceId())
	if err != nil {
		return nil, err
	}
	if !ok {
		return &pb.GetInstanceResponse{Found: false}, nil
	}
	return &pb.GetInstanceResponse{Found: true, Instance: instanceToProto(instance)}, nil
}

func (s *testControlPlaneService) ListInstances(ctx context.Context, _ string, request *pb.ListInstancesRequest) (*pb.ListInstancesResponse, error) {
	instances, err := s.store.ListInstances(ctx, InstanceFilter{
		Profile: request.GetProfile(),
		NodeID:  request.GetNodeId(),
		State:   InstanceState(request.GetState()),
	})
	if err != nil {
		return nil, err
	}
	response := &pb.ListInstancesResponse{Instances: make([]*pb.InstanceView, 0, len(instances))}
	for _, instance := range instances {
		response.Instances = append(response.Instances, instanceToProto(instance))
	}
	return response, nil
}

func (s *testControlPlaneService) RemoveInstance(ctx context.Context, _ string, request *pb.RemoveInstanceRequest) error {
	return s.store.RemoveInstance(ctx, request.GetInstanceId())
}

func (s *testControlPlaneService) AcquireLeader(ctx context.Context, _ string, request *pb.AcquireLeaderRequest) (*pb.AcquireLeaderResponse, error) {
	lease, acquired, err := s.store.AcquireLeader(ctx, request.GetCandidateId(), request.GetCandidateAddress(), time.Duration(request.GetTtlNanos()))
	if err != nil {
		return nil, err
	}
	return &pb.AcquireLeaderResponse{
		Acquired: acquired,
		Leader:   leaderLeaseToProto(lease),
	}, nil
}

func (s *testControlPlaneService) RenewLeader(ctx context.Context, _ string, request *pb.RenewLeaderRequest) (*pb.RenewLeaderResponse, error) {
	lease, err := s.store.RenewLeader(ctx, leaderLeaseFromProto(request.GetLease()), time.Duration(request.GetTtlNanos()))
	if err != nil {
		return nil, err
	}
	return &pb.RenewLeaderResponse{Lease: leaderLeaseToProto(lease)}, nil
}

func (s *testControlPlaneService) ReleaseLeader(ctx context.Context, _ string, request *pb.ReleaseLeaderRequest) error {
	return s.store.ReleaseLeader(ctx, leaderLeaseFromProto(request.GetLease()))
}

func (s *testControlPlaneService) GetLeader(ctx context.Context, _ string, _ *pb.GetLeaderRequest) (*pb.GetLeaderResponse, error) {
	lease, ok, err := s.store.GetLeader(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return &pb.GetLeaderResponse{Found: false}, nil
	}
	return &pb.GetLeaderResponse{
		Found: true,
		Leader: &pb.LeaderLeaseView{
			Fabric:                  lease.Fabric,
			HolderId:                lease.HolderID,
			HolderAddress:           lease.HolderAddress,
			Epoch:                   lease.Epoch,
			AcquiredAtUnixNano:      lease.AcquiredAt.UnixNano(),
			LastHeartbeatAtUnixNano: lease.LastHeartbeatAt.UnixNano(),
			ExpiresAtUnixNano:       lease.ExpiresAt.UnixNano(),
		},
	}, nil
}

func (s *testControlPlaneService) ResolveEndpoint(ctx context.Context, _ string, request *pb.ResolveEndpointRequest) (*pb.ResolveEndpointResponse, error) {
	endpoint, ok, err := ResolveEndpoint(ctx, s.store, request.GetName())
	if err != nil {
		return nil, err
	}
	if !ok {
		return &pb.ResolveEndpointResponse{Found: false}, nil
	}
	return &pb.ResolveEndpointResponse{
		Found:   true,
		Address: endpoint.Address,
		Local:   endpoint.Local,
	}, nil
}

func (s *testControlPlaneService) ListParticipants(ctx context.Context, _ string, _ *pb.ListParticipantsRequest) (*pb.ListParticipantsResponse, error) {
	names, err := ListParticipants(ctx, s.store)
	if err != nil {
		return nil, err
	}
	return &pb.ListParticipantsResponse{Names: names}, nil
}

func (s *testControlPlaneService) UpsertRequest(ctx context.Context, _ string, request *pb.UpsertRequestRequest) error {
	return s.store.UpsertRequest(ctx, requestRecordFromProto(request.GetRequest()))
}

func (s *testControlPlaneService) GetRequest(ctx context.Context, _ string, request *pb.GetRequestRequest) (*pb.GetRequestResponse, error) {
	record, ok, err := s.store.GetRequest(ctx, request.GetRequestId())
	if err != nil {
		return nil, err
	}
	if !ok {
		return &pb.GetRequestResponse{Found: false}, nil
	}
	return &pb.GetRequestResponse{Found: true, Request: requestRecordToProto(record)}, nil
}

func (s *testControlPlaneService) ListRequests(ctx context.Context, _ string, _ *pb.ListRequestsRequest) (*pb.ListRequestsResponse, error) {
	records, err := s.store.ListRequests(ctx)
	if err != nil {
		return nil, err
	}
	response := &pb.ListRequestsResponse{Requests: make([]*pb.RequestRecordView, 0, len(records))}
	for _, record := range records {
		response.Requests = append(response.Requests, requestRecordToProto(record))
	}
	return response, nil
}

func (s *testControlPlaneService) UpsertTask(ctx context.Context, _ string, request *pb.UpsertTaskRequest) error {
	return s.store.UpsertTask(ctx, taskRecordFromProto(request.GetTask()))
}

func (s *testControlPlaneService) ListTasks(ctx context.Context, _ string, request *pb.ListTasksRequest) (*pb.ListTasksResponse, error) {
	records, err := s.store.ListTasks(ctx, request.GetRequestId())
	if err != nil {
		return nil, err
	}
	response := &pb.ListTasksResponse{Tasks: make([]*pb.TaskRecordView, 0, len(records))}
	for _, record := range records {
		response.Tasks = append(response.Tasks, taskRecordToProto(record))
	}
	return response, nil
}

func (s *testControlPlaneService) GetProfileLease(ctx context.Context, _ string, request *pb.GetProfileLeaseRequest) (*pb.GetProfileLeaseResponse, error) {
	lease, ok, err := s.store.GetProfileLease(ctx, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if !ok {
		return &pb.GetProfileLeaseResponse{Found: false}, nil
	}
	return &pb.GetProfileLeaseResponse{Found: true, Lease: profileLeaseToProto(lease)}, nil
}

func (s *testControlPlaneService) AcquireProfileLease(ctx context.Context, _ string, request *pb.AcquireProfileLeaseRequest) (*pb.AcquireProfileLeaseResponse, error) {
	lease, acquired, err := s.store.AcquireProfileLease(ctx, profileLeaseFromProto(request.GetLease()), time.Duration(request.GetTtlNanos()))
	if err != nil {
		return nil, err
	}
	return &pb.AcquireProfileLeaseResponse{Acquired: acquired, Lease: profileLeaseToProto(lease)}, nil
}

func (s *testControlPlaneService) RenewProfileLease(ctx context.Context, _ string, request *pb.RenewProfileLeaseRequest) (*pb.RenewProfileLeaseResponse, error) {
	lease, err := s.store.RenewProfileLease(ctx, profileLeaseFromProto(request.GetLease()), time.Duration(request.GetTtlNanos()))
	if err != nil {
		return nil, err
	}
	return &pb.RenewProfileLeaseResponse{Lease: profileLeaseToProto(lease)}, nil
}

func (s *testControlPlaneService) ReleaseProfileLease(ctx context.Context, _ string, request *pb.ReleaseProfileLeaseRequest) error {
	return s.store.ReleaseProfileLease(ctx, profileLeaseFromProto(request.GetLease()))
}

func (s *testControlPlaneService) GetTaskLease(ctx context.Context, _ string, request *pb.GetTaskLeaseRequest) (*pb.GetTaskLeaseResponse, error) {
	lease, ok, err := s.store.GetTaskLease(ctx, request.GetRequestId(), request.GetTaskId())
	if err != nil {
		return nil, err
	}
	if !ok {
		return &pb.GetTaskLeaseResponse{Found: false}, nil
	}
	return &pb.GetTaskLeaseResponse{Found: true, Lease: taskLeaseToProto(lease)}, nil
}

func (s *testControlPlaneService) AcquireTaskLease(ctx context.Context, _ string, request *pb.AcquireTaskLeaseRequest) (*pb.AcquireTaskLeaseResponse, error) {
	lease, acquired, err := s.store.AcquireTaskLease(ctx, taskLeaseFromProto(request.GetLease()), time.Duration(request.GetTtlNanos()))
	if err != nil {
		return nil, err
	}
	return &pb.AcquireTaskLeaseResponse{Acquired: acquired, Lease: taskLeaseToProto(lease)}, nil
}

func (s *testControlPlaneService) RenewTaskLease(ctx context.Context, _ string, request *pb.RenewTaskLeaseRequest) (*pb.RenewTaskLeaseResponse, error) {
	lease, err := s.store.RenewTaskLease(ctx, taskLeaseFromProto(request.GetLease()), time.Duration(request.GetTtlNanos()))
	if err != nil {
		return nil, err
	}
	return &pb.RenewTaskLeaseResponse{Lease: taskLeaseToProto(lease)}, nil
}

func (s *testControlPlaneService) ReleaseTaskLease(ctx context.Context, _ string, request *pb.ReleaseTaskLeaseRequest) error {
	return s.store.ReleaseTaskLease(ctx, taskLeaseFromProto(request.GetLease()))
}

func nodeToProto(node Node) *pb.NodeView {
	return &pb.NodeView{
		Id:                      node.ID,
		Address:                 node.Address,
		State:                   string(node.State),
		Labels:                  node.Labels,
		BundleDigest:            node.BundleDigest,
		ProfileDigests:          node.ProfileDigests,
		MaxInstances:            int32(node.Capacity.MaxInstances),
		MaxTasks:                int32(node.Capacity.MaxTasks),
		StartedAtUnixNano:       node.StartedAt.UnixNano(),
		LastHeartbeatAtUnixNano: node.LastHeartbeatAt.UnixNano(),
	}
}

func instanceToProto(instance AgentInstance) *pb.InstanceView {
	return &pb.InstanceView{
		Id:                      instance.ID,
		Profile:                 instance.Profile,
		NodeId:                  instance.NodeID,
		BundleDigest:            instance.BundleDigest,
		ProfileDigest:           instance.ProfileDigest,
		EndpointAddress:         instance.Endpoint.Address,
		EndpointLocal:           instance.Endpoint.Local,
		State:                   string(instance.State),
		StartedAtUnixNano:       instance.StartedAt.UnixNano(),
		LastHeartbeatAtUnixNano: instance.LastHeartbeatAt.UnixNano(),
	}
}

func TestRemoteClientResolvesAndReadsThroughControlPlaneAPI(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore("test-fabric")

	provider, err := identity.NewLocalDevProvider("agentfab.local")
	if err != nil {
		t.Fatalf("NewLocalDevProvider: %v", err)
	}

	conductorIdentity, err := provider.IssueCertificate(ctx, identity.IssueRequest{
		Subject: identity.Subject{
			TrustDomain: "agentfab.local",
			Fabric:      "test-fabric",
			Kind:        identity.SubjectKindConductor,
			Name:        "conductor",
		},
		Principal: "conductor",
		IPAddresses: []net.IP{
			net.ParseIP("127.0.0.1"),
		},
	})
	if err != nil {
		t.Fatalf("IssueCertificate conductor: %v", err)
	}
	nodeIdentity, err := provider.IssueCertificate(ctx, identity.IssueRequest{
		Subject: identity.Subject{
			TrustDomain: "agentfab.local",
			Fabric:      "test-fabric",
			Kind:        identity.SubjectKindNode,
			Name:        "node-a",
			NodeID:      "node-a",
		},
		Principal: "node-a",
		IPAddresses: []net.IP{
			net.ParseIP("127.0.0.1"),
		},
	})
	if err != nil {
		t.Fatalf("IssueCertificate node: %v", err)
	}

	server, err := agentgrpc.NewServer("conductor", "127.0.0.1:0", 16, conductorIdentity.ServerTLS)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	server.SetControlPlaneService(&testControlPlaneService{store: store})
	go func() {
		_ = server.Serve()
	}()
	defer server.Stop()

	if _, acquired, err := store.AcquireLeader(ctx, "conductor-a", server.Addr(), time.Minute); err != nil {
		t.Fatalf("AcquireLeader: %v", err)
	} else if !acquired {
		t.Fatal("expected leader acquisition to succeed")
	}
	if err := store.RegisterInstance(ctx, AgentInstance{
		ID:              "node-a/developer/1",
		Profile:         "developer",
		NodeID:          "node-a",
		Endpoint:        runtime.Endpoint{Address: "127.0.0.1:6001"},
		State:           InstanceStateReady,
		LastHeartbeatAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RegisterInstance: %v", err)
	}
	if err := store.UpsertRequest(ctx, RequestRecord{
		ID:          "req-1",
		State:       RequestStateRunning,
		UserRequest: "design an api",
		LeaderID:    "conductor-a",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("UpsertRequest: %v", err)
	}
	if err := store.UpsertTask(ctx, TaskRecord{
		RequestID: "req-1",
		TaskID:    "task-1",
		Profile:   "developer",
		Status:    "running",
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	if _, acquired, err := store.AcquireTaskLease(ctx, TaskLease{
		RequestID: "req-1",
		TaskID:    "task-1",
		Profile:   "developer",
		OwnerID:   "conductor-a",
	}, time.Minute); err != nil {
		t.Fatalf("AcquireTaskLease: %v", err)
	} else if !acquired {
		t.Fatal("expected task lease acquisition to succeed")
	}

	client := NewRemoteClient(server.Addr(), nodeIdentity.ClientTLS)

	acquiredLease, acquired, err := client.AcquireLeader(ctx, "conductor-b", server.Addr(), time.Minute)
	if err != nil {
		t.Fatalf("AcquireLeader through API: %v", err)
	}
	if acquired {
		t.Fatal("expected leader acquisition to be rejected while leader is held")
	}
	if acquiredLease.HolderAddress != server.Addr() {
		t.Fatalf("acquired lease holder address = %q, want %q", acquiredLease.HolderAddress, server.Addr())
	}

	leader, ok, err := client.GetLeader(ctx)
	if err != nil {
		t.Fatalf("GetLeader: %v", err)
	}
	if !ok {
		t.Fatal("expected leader to be returned")
	}
	if leader.HolderAddress != server.Addr() {
		t.Fatalf("leader address = %q, want %q", leader.HolderAddress, server.Addr())
	}

	endpoint, err := client.Resolve(ctx, "developer")
	if err != nil {
		t.Fatalf("Resolve developer: %v", err)
	}
	if endpoint.Address != "127.0.0.1:6001" {
		t.Fatalf("endpoint address = %q, want 127.0.0.1:6001", endpoint.Address)
	}

	conductorEndpoint, err := client.Resolve(ctx, "conductor")
	if err != nil {
		t.Fatalf("Resolve conductor: %v", err)
	}
	if conductorEndpoint.Address != server.Addr() {
		t.Fatalf("conductor endpoint address = %q, want %q", conductorEndpoint.Address, server.Addr())
	}

	names, err := client.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) < 2 {
		t.Fatalf("names = %v, want at least conductor and developer", names)
	}

	request, ok, err := client.GetRequest(ctx, "req-1")
	if err != nil {
		t.Fatalf("GetRequest: %v", err)
	}
	if !ok || request.State != RequestStateRunning {
		t.Fatalf("request = %+v, ok=%t, want running request", request, ok)
	}

	requests, err := client.ListRequests(ctx)
	if err != nil {
		t.Fatalf("ListRequests: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(requests))
	}

	tasks, err := client.ListTasks(ctx, "req-1")
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].TaskID != "task-1" {
		t.Fatalf("tasks = %+v, want task-1", tasks)
	}

	if err := client.UpsertRequest(ctx, RequestRecord{
		ID:          "req-2",
		State:       RequestStatePending,
		UserRequest: "new request",
		LeaderID:    "conductor-a",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("UpsertRequest through API: %v", err)
	}
	if err := client.UpsertTask(ctx, TaskRecord{
		RequestID: "req-2",
		TaskID:    "task-2",
		Profile:   "architect",
		Status:    "pending",
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("UpsertTask through API: %v", err)
	}
	leased, acquired, err := client.AcquireTaskLease(ctx, TaskLease{
		RequestID: "req-2",
		TaskID:    "task-2",
		Profile:   "architect",
		OwnerID:   "conductor-a",
	}, time.Minute)
	if err != nil {
		t.Fatalf("AcquireTaskLease through API: %v", err)
	}
	if !acquired {
		t.Fatal("expected task lease acquisition through API to succeed")
	}
	if _, err := client.RenewTaskLease(ctx, leased, time.Minute); err != nil {
		t.Fatalf("RenewTaskLease through API: %v", err)
	}
	if err := client.ReleaseTaskLease(ctx, leased); err != nil {
		t.Fatalf("ReleaseTaskLease through API: %v", err)
	}

	taskLease, ok, err := client.GetTaskLease(ctx, "req-1", "task-1")
	if err != nil {
		t.Fatalf("GetTaskLease: %v", err)
	}
	if !ok || taskLease.OwnerID != "conductor-a" {
		t.Fatalf("task lease = %+v, ok=%t, want owner conductor-a", taskLease, ok)
	}
}
