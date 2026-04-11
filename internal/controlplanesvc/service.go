package controlplanesvc

import (
	"context"
	"fmt"
	"time"

	pb "github.com/razvanmaftei/agentfab/gen/agentfab/v1"
	"github.com/razvanmaftei/agentfab/internal/controlplane"
	"github.com/razvanmaftei/agentfab/internal/identity"
	"github.com/razvanmaftei/agentfab/internal/runtime"
)

type Config struct {
	Store                  controlplane.Store
	Fabric                 string
	ExpectedBundleDigest   string
	ExpectedProfileDigests map[string]string
	Attestor               identity.Attestor
}

type Service struct {
	store                  controlplane.Store
	fabric                 string
	expectedBundleDigest   string
	expectedProfileDigests map[string]string
	attestor               identity.Attestor
}

func New(config Config) *Service {
	return &Service{
		store:                  config.Store,
		fabric:                 config.Fabric,
		expectedBundleDigest:   config.ExpectedBundleDigest,
		expectedProfileDigests: cloneLabels(config.ExpectedProfileDigests),
		attestor:               config.Attestor,
	}
}

func (s *Service) AttestNode(ctx context.Context, subjectURI string, request *pb.AttestNodeRequest) (*pb.AttestNodeResponse, error) {
	if s.attestor == nil {
		return nil, fmt.Errorf("node attestor is not configured")
	}
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeNodeSubject(subject, subject.NodeID); err != nil {
		return nil, err
	}

	claims := cloneLabels(request.GetClaims())
	if claims == nil {
		claims = make(map[string]string)
	}
	if claims["node_id"] == "" {
		claims["node_id"] = subject.NodeID
	}
	if claims["fabric"] == "" {
		claims["fabric"] = s.fabric
	}

	attestedNode, err := s.attestor.AttestNode(ctx, identity.NodeAttestation{
		Type:         request.GetType(),
		Claims:       claims,
		Measurements: cloneLabels(request.GetMeasurements()),
		Token:        request.GetToken(),
	})
	if err != nil {
		return nil, err
	}
	if attestedNode.NodeID != "" && attestedNode.NodeID != subject.NodeID {
		return nil, fmt.Errorf("attested node %q does not match subject node %q", attestedNode.NodeID, subject.NodeID)
	}
	attestations, err := s.attestationStore()
	if err != nil {
		return nil, err
	}
	if err := attestations.UpsertAttestedNode(ctx, attestedNodeRecordFromIdentity(attestedNode)); err != nil {
		return nil, err
	}
	return &pb.AttestNodeResponse{
		NodeId:             attestedNode.NodeID,
		TrustDomain:        attestedNode.TrustDomain,
		Claims:             cloneLabels(attestedNode.Claims),
		Measurements:       cloneLabels(attestedNode.Measurements),
		AttestedAtUnixNano: attestedNode.AttestedAt.UnixNano(),
		ExpiresAtUnixNano:  attestedNode.ExpiresAt.UnixNano(),
	}, nil
}

func (s *Service) RegisterNode(ctx context.Context, subjectURI string, request *pb.RegisterNodeRequest) error {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return err
	}
	node := controlplane.Node{
		ID:             request.GetNodeId(),
		Address:        request.GetAddress(),
		State:          controlplane.NodeState(request.GetState()),
		Labels:         cloneLabels(request.GetLabels()),
		BundleDigest:   request.GetBundleDigest(),
		ProfileDigests: cloneLabels(request.GetProfileDigests()),
		Capacity: controlplane.NodeCapacity{
			MaxInstances: int(request.GetMaxInstances()),
			MaxTasks:     int(request.GetMaxTasks()),
		},
		StartedAt:       timeFromUnixNano(request.GetStartedAtUnixNano()),
		LastHeartbeatAt: timeFromUnixNano(request.GetLastHeartbeatAtUnixNano()),
	}
	if err := s.authorizeNodeSubject(subject, node.ID); err != nil {
		if err := s.authorizeConductorNodeSubject(subject, node.ID); err != nil {
			return err
		}
	} else {
		if err := s.requireAttestedNode(ctx, node.ID, node.BundleDigest); err != nil {
			return err
		}
	}
	if err := s.validateNodeBundle(node); err != nil {
		return err
	}
	return s.store.RegisterNode(ctx, node)
}

func (s *Service) HeartbeatNode(ctx context.Context, subjectURI string, request *pb.HeartbeatNodeRequest) error {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return err
	}
	if err := s.authorizeNodeSubject(subject, request.GetNodeId()); err != nil {
		if err := s.authorizeConductorNodeSubject(subject, request.GetNodeId()); err != nil {
			return err
		}
	}
	return s.store.HeartbeatNode(ctx, request.GetNodeId(), timeFromUnixNano(request.GetAtUnixNano()))
}

func (s *Service) GetNode(ctx context.Context, subjectURI string, request *pb.GetNodeRequest) (*pb.GetNodeResponse, error) {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeFabric(subject); err != nil {
		return nil, err
	}

	node, ok, err := s.store.GetNode(ctx, request.GetNodeId())
	if err != nil {
		return nil, err
	}
	if !ok {
		return &pb.GetNodeResponse{Found: false}, nil
	}
	return &pb.GetNodeResponse{Found: true, Node: nodeView(node)}, nil
}

func (s *Service) ListNodes(ctx context.Context, subjectURI string, _ *pb.ListNodesRequest) (*pb.ListNodesResponse, error) {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeFabric(subject); err != nil {
		return nil, err
	}

	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	response := &pb.ListNodesResponse{Nodes: make([]*pb.NodeView, 0, len(nodes))}
	for _, node := range nodes {
		response.Nodes = append(response.Nodes, nodeView(node))
	}
	return response, nil
}

func (s *Service) RegisterInstance(ctx context.Context, subjectURI string, request *pb.RegisterInstanceRequest) error {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return err
	}
	instance := controlplane.AgentInstance{
		ID:            request.GetInstanceId(),
		Profile:       request.GetProfile(),
		NodeID:        request.GetNodeId(),
		BundleDigest:  request.GetBundleDigest(),
		ProfileDigest: request.GetProfileDigest(),
		Endpoint: runtime.Endpoint{
			Address: request.GetEndpointAddress(),
			Local:   request.GetEndpointLocal(),
		},
		State:           controlplane.InstanceState(request.GetState()),
		StartedAt:       timeFromUnixNano(request.GetStartedAtUnixNano()),
		LastHeartbeatAt: timeFromUnixNano(request.GetLastHeartbeatAtUnixNano()),
	}
	if err := s.authorizeInstanceSubject(subject, instance.ID, instance.NodeID, instance.Profile); err != nil {
		return err
	}
	if err := s.requireAttestedNode(ctx, instance.NodeID, instance.BundleDigest); err != nil {
		return err
	}
	if err := s.validateInstanceBundle(instance); err != nil {
		return err
	}
	return s.store.RegisterInstance(ctx, instance)
}

func (s *Service) HeartbeatInstance(ctx context.Context, subjectURI string, request *pb.HeartbeatInstanceRequest) error {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return err
	}
	instance, ok, err := s.store.GetInstance(ctx, request.GetInstanceId())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("instance %q not found", request.GetInstanceId())
	}
	if err := s.authorizeInstanceSubject(subject, instance.ID, instance.NodeID, instance.Profile); err != nil {
		return err
	}
	return s.store.HeartbeatInstance(ctx, request.GetInstanceId(), timeFromUnixNano(request.GetAtUnixNano()))
}

func (s *Service) GetInstance(ctx context.Context, subjectURI string, request *pb.GetInstanceRequest) (*pb.GetInstanceResponse, error) {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeFabric(subject); err != nil {
		return nil, err
	}

	instance, ok, err := s.store.GetInstance(ctx, request.GetInstanceId())
	if err != nil {
		return nil, err
	}
	if !ok {
		return &pb.GetInstanceResponse{Found: false}, nil
	}
	return &pb.GetInstanceResponse{Found: true, Instance: instanceView(instance)}, nil
}

func (s *Service) ListInstances(ctx context.Context, subjectURI string, request *pb.ListInstancesRequest) (*pb.ListInstancesResponse, error) {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeFabric(subject); err != nil {
		return nil, err
	}

	instances, err := s.store.ListInstances(ctx, controlplane.InstanceFilter{
		Profile: request.GetProfile(),
		NodeID:  request.GetNodeId(),
		State:   controlplane.InstanceState(request.GetState()),
	})
	if err != nil {
		return nil, err
	}
	response := &pb.ListInstancesResponse{Instances: make([]*pb.InstanceView, 0, len(instances))}
	for _, instance := range instances {
		response.Instances = append(response.Instances, instanceView(instance))
	}
	return response, nil
}

func (s *Service) RemoveInstance(ctx context.Context, subjectURI string, request *pb.RemoveInstanceRequest) error {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return err
	}
	instance, ok, err := s.store.GetInstance(ctx, request.GetInstanceId())
	if err != nil {
		return err
	}
	if ok {
		if err := s.authorizeInstanceSubject(subject, instance.ID, instance.NodeID, instance.Profile); err != nil {
			return err
		}
	}
	return s.store.RemoveInstance(ctx, request.GetInstanceId())
}

func (s *Service) AcquireLeader(ctx context.Context, subjectURI string, request *pb.AcquireLeaderRequest) (*pb.AcquireLeaderResponse, error) {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeConductorSubject(subject); err != nil {
		return nil, err
	}

	lease, acquired, err := s.store.AcquireLeader(
		ctx,
		request.GetCandidateId(),
		request.GetCandidateAddress(),
		durationFromNanos(request.GetTtlNanos()),
	)
	if err != nil {
		return nil, err
	}
	return &pb.AcquireLeaderResponse{
		Acquired: acquired,
		Leader:   leaderLeaseView(lease),
	}, nil
}

func (s *Service) RenewLeader(ctx context.Context, subjectURI string, request *pb.RenewLeaderRequest) (*pb.RenewLeaderResponse, error) {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeConductorSubject(subject); err != nil {
		return nil, err
	}

	lease, err := s.store.RenewLeader(ctx, leaderLeaseFromView(request.GetLease()), durationFromNanos(request.GetTtlNanos()))
	if err != nil {
		return nil, err
	}
	return &pb.RenewLeaderResponse{Lease: leaderLeaseView(lease)}, nil
}

func (s *Service) ReleaseLeader(ctx context.Context, subjectURI string, request *pb.ReleaseLeaderRequest) error {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return err
	}
	if err := s.authorizeConductorSubject(subject); err != nil {
		return err
	}
	return s.store.ReleaseLeader(ctx, leaderLeaseFromView(request.GetLease()))
}

func (s *Service) GetLeader(ctx context.Context, subjectURI string, _ *pb.GetLeaderRequest) (*pb.GetLeaderResponse, error) {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeFabric(subject); err != nil {
		return nil, err
	}

	lease, ok, err := s.store.GetLeader(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return &pb.GetLeaderResponse{Found: false}, nil
	}

	return &pb.GetLeaderResponse{
		Found:  true,
		Leader: leaderLeaseView(lease),
	}, nil
}

func (s *Service) ResolveEndpoint(ctx context.Context, subjectURI string, request *pb.ResolveEndpointRequest) (*pb.ResolveEndpointResponse, error) {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeFabric(subject); err != nil {
		return nil, err
	}

	endpoint, ok, err := controlplane.ResolveEndpoint(ctx, s.store, request.GetName())
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

func (s *Service) ListParticipants(ctx context.Context, subjectURI string, _ *pb.ListParticipantsRequest) (*pb.ListParticipantsResponse, error) {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeFabric(subject); err != nil {
		return nil, err
	}

	names, err := controlplane.ListParticipants(ctx, s.store)
	if err != nil {
		return nil, err
	}
	return &pb.ListParticipantsResponse{Names: names}, nil
}

func (s *Service) UpsertRequest(ctx context.Context, subjectURI string, request *pb.UpsertRequestRequest) error {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return err
	}
	if err := s.authorizeConductorSubject(subject); err != nil {
		return err
	}
	return s.store.UpsertRequest(ctx, requestRecordFromView(request.GetRequest()))
}

func (s *Service) GetRequest(ctx context.Context, subjectURI string, request *pb.GetRequestRequest) (*pb.GetRequestResponse, error) {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeFabric(subject); err != nil {
		return nil, err
	}

	record, ok, err := s.store.GetRequest(ctx, request.GetRequestId())
	if err != nil {
		return nil, err
	}
	if !ok {
		return &pb.GetRequestResponse{Found: false}, nil
	}
	return &pb.GetRequestResponse{
		Found:   true,
		Request: requestRecordView(record),
	}, nil
}

func (s *Service) ListRequests(ctx context.Context, subjectURI string, _ *pb.ListRequestsRequest) (*pb.ListRequestsResponse, error) {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeFabric(subject); err != nil {
		return nil, err
	}

	records, err := s.store.ListRequests(ctx)
	if err != nil {
		return nil, err
	}
	response := &pb.ListRequestsResponse{
		Requests: make([]*pb.RequestRecordView, 0, len(records)),
	}
	for _, record := range records {
		response.Requests = append(response.Requests, requestRecordView(record))
	}
	return response, nil
}

func (s *Service) UpsertTask(ctx context.Context, subjectURI string, request *pb.UpsertTaskRequest) error {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return err
	}
	if err := s.authorizeConductorSubject(subject); err != nil {
		return err
	}
	return s.store.UpsertTask(ctx, taskRecordFromView(request.GetTask()))
}

func (s *Service) ListTasks(ctx context.Context, subjectURI string, request *pb.ListTasksRequest) (*pb.ListTasksResponse, error) {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeFabric(subject); err != nil {
		return nil, err
	}

	records, err := s.store.ListTasks(ctx, request.GetRequestId())
	if err != nil {
		return nil, err
	}
	response := &pb.ListTasksResponse{
		Tasks: make([]*pb.TaskRecordView, 0, len(records)),
	}
	for _, record := range records {
		response.Tasks = append(response.Tasks, taskRecordView(record))
	}
	return response, nil
}

func (s *Service) AcquireTaskLease(ctx context.Context, subjectURI string, request *pb.AcquireTaskLeaseRequest) (*pb.AcquireTaskLeaseResponse, error) {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeConductorSubject(subject); err != nil {
		return nil, err
	}

	lease, acquired, err := s.store.AcquireTaskLease(ctx, taskLeaseFromView(request.GetLease()), durationFromNanos(request.GetTtlNanos()))
	if err != nil {
		return nil, err
	}
	return &pb.AcquireTaskLeaseResponse{
		Acquired: acquired,
		Lease:    taskLeaseView(lease),
	}, nil
}

func (s *Service) AcquireProfileLease(ctx context.Context, subjectURI string, request *pb.AcquireProfileLeaseRequest) (*pb.AcquireProfileLeaseResponse, error) {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeConductorSubject(subject); err != nil {
		return nil, err
	}

	lease, acquired, err := s.store.AcquireProfileLease(ctx, profileLeaseFromView(request.GetLease()), durationFromNanos(request.GetTtlNanos()))
	if err != nil {
		return nil, err
	}
	return &pb.AcquireProfileLeaseResponse{
		Acquired: acquired,
		Lease:    profileLeaseView(lease),
	}, nil
}

func (s *Service) RenewProfileLease(ctx context.Context, subjectURI string, request *pb.RenewProfileLeaseRequest) (*pb.RenewProfileLeaseResponse, error) {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeConductorSubject(subject); err != nil {
		return nil, err
	}

	lease, err := s.store.RenewProfileLease(ctx, profileLeaseFromView(request.GetLease()), durationFromNanos(request.GetTtlNanos()))
	if err != nil {
		return nil, err
	}
	return &pb.RenewProfileLeaseResponse{
		Lease: profileLeaseView(lease),
	}, nil
}

func (s *Service) GetProfileLease(ctx context.Context, subjectURI string, request *pb.GetProfileLeaseRequest) (*pb.GetProfileLeaseResponse, error) {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeFabric(subject); err != nil {
		return nil, err
	}

	lease, ok, err := s.store.GetProfileLease(ctx, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if !ok {
		return &pb.GetProfileLeaseResponse{Found: false}, nil
	}
	return &pb.GetProfileLeaseResponse{
		Found: true,
		Lease: profileLeaseView(lease),
	}, nil
}

func (s *Service) ReleaseProfileLease(ctx context.Context, subjectURI string, request *pb.ReleaseProfileLeaseRequest) error {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return err
	}
	if err := s.authorizeConductorSubject(subject); err != nil {
		return err
	}
	return s.store.ReleaseProfileLease(ctx, profileLeaseFromView(request.GetLease()))
}

func (s *Service) RenewTaskLease(ctx context.Context, subjectURI string, request *pb.RenewTaskLeaseRequest) (*pb.RenewTaskLeaseResponse, error) {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeConductorSubject(subject); err != nil {
		return nil, err
	}

	lease, err := s.store.RenewTaskLease(ctx, taskLeaseFromView(request.GetLease()), durationFromNanos(request.GetTtlNanos()))
	if err != nil {
		return nil, err
	}
	return &pb.RenewTaskLeaseResponse{
		Lease: taskLeaseView(lease),
	}, nil
}

func (s *Service) GetTaskLease(ctx context.Context, subjectURI string, request *pb.GetTaskLeaseRequest) (*pb.GetTaskLeaseResponse, error) {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeFabric(subject); err != nil {
		return nil, err
	}

	lease, ok, err := s.store.GetTaskLease(ctx, request.GetRequestId(), request.GetTaskId())
	if err != nil {
		return nil, err
	}
	if !ok {
		return &pb.GetTaskLeaseResponse{Found: false}, nil
	}
	return &pb.GetTaskLeaseResponse{
		Found: true,
		Lease: taskLeaseView(lease),
	}, nil
}

func (s *Service) ReleaseTaskLease(ctx context.Context, subjectURI string, request *pb.ReleaseTaskLeaseRequest) error {
	subject, err := identity.ParseSubjectURI(subjectURI)
	if err != nil {
		return err
	}
	if err := s.authorizeConductorSubject(subject); err != nil {
		return err
	}
	return s.store.ReleaseTaskLease(ctx, taskLeaseFromView(request.GetLease()))
}

func (s *Service) authorizeNodeSubject(subject identity.Subject, nodeID string) error {
	if err := s.authorizeFabric(subject); err != nil {
		return err
	}
	if subject.Kind != identity.SubjectKindNode {
		return fmt.Errorf("subject %q is not authorized for node membership", subject.Kind)
	}
	if subject.NodeID != identity.NormalizeID(nodeID) {
		return fmt.Errorf("node subject %q cannot act on node %q", subject.NodeID, nodeID)
	}
	return nil
}

func (s *Service) authorizeConductorSubject(subject identity.Subject) error {
	if err := s.authorizeFabric(subject); err != nil {
		return err
	}
	if subject.Kind != identity.SubjectKindConductor {
		return fmt.Errorf("subject %q is not authorized for conductor leadership", subject.Kind)
	}
	return nil
}

func (s *Service) authorizeConductorNodeSubject(subject identity.Subject, nodeID string) error {
	if err := s.authorizeConductorSubject(subject); err != nil {
		return err
	}
	if subject.Name != identity.NormalizeID(nodeID) {
		return fmt.Errorf("conductor subject %q cannot act on node %q", subject.Name, nodeID)
	}
	return nil
}

func (s *Service) authorizeInstanceSubject(subject identity.Subject, instanceID, nodeID, profile string) error {
	if err := s.authorizeFabric(subject); err != nil {
		return err
	}
	switch subject.Kind {
	case identity.SubjectKindAgentInstance:
		if subject.InstanceID != identity.NormalizeID(instanceID) {
			return fmt.Errorf("instance subject %q cannot act on instance %q", subject.InstanceID, instanceID)
		}
		if subject.NodeID != identity.NormalizeID(nodeID) {
			return fmt.Errorf("instance subject node %q cannot act on node %q", subject.NodeID, nodeID)
		}
		if subject.Profile != identity.NormalizeID(profile) {
			return fmt.Errorf("instance subject profile %q cannot act on profile %q", subject.Profile, profile)
		}
		return nil
	case identity.SubjectKindNode:
		if subject.NodeID != identity.NormalizeID(nodeID) {
			return fmt.Errorf("node subject %q cannot act on node %q", subject.NodeID, nodeID)
		}
		return nil
	default:
		return fmt.Errorf("subject %q is not authorized for instance membership", subject.Kind)
	}
}

func (s *Service) authorizeFabric(subject identity.Subject) error {
	if s.fabric != "" && subject.Fabric != s.fabric {
		return fmt.Errorf("subject fabric %q is not authorized for fabric %q", subject.Fabric, s.fabric)
	}
	return nil
}

func (s *Service) requireAttestedNode(ctx context.Context, nodeID, bundleDigest string) error {
	attestations, err := s.attestationStore()
	if err != nil {
		return err
	}
	record, ok, err := attestations.GetAttestedNode(ctx, nodeID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("node %q has not been attested by the control plane", nodeID)
	}
	if !record.ExpiresAt.IsZero() && time.Now().UTC().After(record.ExpiresAt) {
		return fmt.Errorf("node %q attestation has expired", nodeID)
	}
	if bundleDigest == "" {
		return nil
	}
	if measuredBundle := record.Measurements["bundle_digest"]; measuredBundle != "" && measuredBundle != bundleDigest {
		return fmt.Errorf("node %q attested bundle digest %q does not match registered bundle digest %q", nodeID, measuredBundle, bundleDigest)
	}
	return nil
}

func (s *Service) attestationStore() (controlplane.Attestations, error) {
	store, ok := s.store.(controlplane.Attestations)
	if !ok {
		return nil, fmt.Errorf("control plane store does not implement attestation persistence")
	}
	return store, nil
}

func (s *Service) validateNodeBundle(node controlplane.Node) error {
	if s.expectedBundleDigest == "" {
		return nil
	}
	if node.BundleDigest == "" {
		return fmt.Errorf("node %q did not provide a bundle digest", node.ID)
	}
	if node.BundleDigest != s.expectedBundleDigest {
		return fmt.Errorf("node %q bundle digest %q does not match fabric bundle %q", node.ID, node.BundleDigest, s.expectedBundleDigest)
	}
	for profile, digest := range node.ProfileDigests {
		expectedDigest, ok := s.expectedProfileDigests[profile]
		if !ok {
			return fmt.Errorf("node %q advertised unknown profile %q", node.ID, profile)
		}
		if digest == "" {
			return fmt.Errorf("node %q advertised profile %q without a profile digest", node.ID, profile)
		}
		if digest != expectedDigest {
			return fmt.Errorf("node %q profile %q digest %q does not match fabric profile digest %q", node.ID, profile, digest, expectedDigest)
		}
	}
	return nil
}

func (s *Service) validateInstanceBundle(instance controlplane.AgentInstance) error {
	expectedDigest, ok := s.expectedProfileDigests[instance.Profile]
	if !ok {
		return fmt.Errorf("instance %q uses unknown profile %q", instance.ID, instance.Profile)
	}
	if s.expectedBundleDigest != "" {
		if instance.BundleDigest == "" {
			return fmt.Errorf("instance %q did not provide a bundle digest", instance.ID)
		}
		if instance.BundleDigest != s.expectedBundleDigest {
			return fmt.Errorf("instance %q bundle digest %q does not match fabric bundle %q", instance.ID, instance.BundleDigest, s.expectedBundleDigest)
		}
	}
	if instance.ProfileDigest == "" {
		return fmt.Errorf("instance %q did not provide a profile digest", instance.ID)
	}
	if instance.ProfileDigest != expectedDigest {
		return fmt.Errorf("instance %q profile %q digest %q does not match fabric profile digest %q", instance.ID, instance.Profile, instance.ProfileDigest, expectedDigest)
	}
	return nil
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

func attestedNodeRecordFromIdentity(node identity.AttestedNode) controlplane.AttestedNodeRecord {
	return controlplane.AttestedNodeRecord{
		NodeID:       node.NodeID,
		TrustDomain:  node.TrustDomain,
		Claims:       cloneLabels(node.Claims),
		Measurements: cloneLabels(node.Measurements),
		AttestedAt:   node.AttestedAt,
		ExpiresAt:    node.ExpiresAt,
	}
}

func timeFromUnixNano(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, value).UTC()
}

func durationFromNanos(value int64) time.Duration {
	if value <= 0 {
		return 0
	}
	return time.Duration(value)
}

func leaderLeaseView(lease controlplane.LeaderLease) *pb.LeaderLeaseView {
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

func leaderLeaseFromView(view *pb.LeaderLeaseView) controlplane.LeaderLease {
	if view == nil {
		return controlplane.LeaderLease{}
	}
	return controlplane.LeaderLease{
		Fabric:          view.GetFabric(),
		HolderID:        view.GetHolderId(),
		HolderAddress:   view.GetHolderAddress(),
		Epoch:           view.GetEpoch(),
		AcquiredAt:      timeFromUnixNano(view.GetAcquiredAtUnixNano()),
		LastHeartbeatAt: timeFromUnixNano(view.GetLastHeartbeatAtUnixNano()),
		ExpiresAt:       timeFromUnixNano(view.GetExpiresAtUnixNano()),
	}
}

func nodeView(node controlplane.Node) *pb.NodeView {
	return &pb.NodeView{
		Id:                      node.ID,
		Address:                 node.Address,
		State:                   string(node.State),
		Labels:                  cloneLabels(node.Labels),
		BundleDigest:            node.BundleDigest,
		ProfileDigests:          cloneLabels(node.ProfileDigests),
		MaxInstances:            int32(node.Capacity.MaxInstances),
		MaxTasks:                int32(node.Capacity.MaxTasks),
		StartedAtUnixNano:       node.StartedAt.UnixNano(),
		LastHeartbeatAtUnixNano: node.LastHeartbeatAt.UnixNano(),
	}
}

func nodeFromView(view *pb.NodeView) controlplane.Node {
	if view == nil {
		return controlplane.Node{}
	}
	return controlplane.Node{
		ID:             view.GetId(),
		Address:        view.GetAddress(),
		State:          controlplane.NodeState(view.GetState()),
		Labels:         cloneLabels(view.GetLabels()),
		BundleDigest:   view.GetBundleDigest(),
		ProfileDigests: cloneLabels(view.GetProfileDigests()),
		Capacity: controlplane.NodeCapacity{
			MaxInstances: int(view.GetMaxInstances()),
			MaxTasks:     int(view.GetMaxTasks()),
		},
		StartedAt:       timeFromUnixNano(view.GetStartedAtUnixNano()),
		LastHeartbeatAt: timeFromUnixNano(view.GetLastHeartbeatAtUnixNano()),
	}
}

func instanceView(instance controlplane.AgentInstance) *pb.InstanceView {
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

func instanceFromView(view *pb.InstanceView) controlplane.AgentInstance {
	if view == nil {
		return controlplane.AgentInstance{}
	}
	return controlplane.AgentInstance{
		ID:            view.GetId(),
		Profile:       view.GetProfile(),
		NodeID:        view.GetNodeId(),
		BundleDigest:  view.GetBundleDigest(),
		ProfileDigest: view.GetProfileDigest(),
		Endpoint: runtime.Endpoint{
			Address: view.GetEndpointAddress(),
			Local:   view.GetEndpointLocal(),
		},
		State:           controlplane.InstanceState(view.GetState()),
		StartedAt:       timeFromUnixNano(view.GetStartedAtUnixNano()),
		LastHeartbeatAt: timeFromUnixNano(view.GetLastHeartbeatAtUnixNano()),
	}
}

func requestRecordView(record controlplane.RequestRecord) *pb.RequestRecordView {
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

func requestRecordFromView(view *pb.RequestRecordView) controlplane.RequestRecord {
	if view == nil {
		return controlplane.RequestRecord{}
	}
	return controlplane.RequestRecord{
		ID:           view.GetId(),
		State:        controlplane.RequestState(view.GetState()),
		UserRequest:  view.GetUserRequest(),
		GraphVersion: view.GetGraphVersion(),
		LeaderID:     view.GetLeaderId(),
		CreatedAt:    timeFromUnixNano(view.GetCreatedAtUnixNano()),
		UpdatedAt:    timeFromUnixNano(view.GetUpdatedAtUnixNano()),
	}
}

func taskRecordView(record controlplane.TaskRecord) *pb.TaskRecordView {
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

func taskRecordFromView(view *pb.TaskRecordView) controlplane.TaskRecord {
	if view == nil {
		return controlplane.TaskRecord{}
	}
	return controlplane.TaskRecord{
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

func taskLeaseView(lease controlplane.TaskLease) *pb.TaskLeaseView {
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

func profileLeaseView(lease controlplane.ProfileLease) *pb.ProfileLeaseView {
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

func profileLeaseFromView(view *pb.ProfileLeaseView) controlplane.ProfileLease {
	if view == nil {
		return controlplane.ProfileLease{}
	}
	return controlplane.ProfileLease{
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

func taskLeaseFromView(view *pb.TaskLeaseView) controlplane.TaskLease {
	if view == nil {
		return controlplane.TaskLease{}
	}
	return controlplane.TaskLease{
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
