package grpc

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync/atomic"

	pb "github.com/razvanmaftei/agentfab/gen/agentfab/v1"
	"github.com/razvanmaftei/agentfab/internal/identity"
	"github.com/razvanmaftei/agentfab/internal/message"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Server receives messages for a single participant via gRPC.
type Server struct {
	pb.UnimplementedAgentServiceServer
	pb.UnimplementedControlPlaneServiceServer

	name         string
	inbox        chan *message.Message
	grpcServer   *grpc.Server
	listener     net.Listener
	status       atomic.Value // string: "ready", "busy", "shutting_down"
	controlPlane ControlPlaneService
}

type ControlPlaneService interface {
	AttestNode(ctx context.Context, subjectURI string, request *pb.AttestNodeRequest) (*pb.AttestNodeResponse, error)
	RegisterNode(ctx context.Context, subjectURI string, request *pb.RegisterNodeRequest) error
	HeartbeatNode(ctx context.Context, subjectURI string, request *pb.HeartbeatNodeRequest) error
	GetNode(ctx context.Context, subjectURI string, request *pb.GetNodeRequest) (*pb.GetNodeResponse, error)
	ListNodes(ctx context.Context, subjectURI string, request *pb.ListNodesRequest) (*pb.ListNodesResponse, error)
	RegisterInstance(ctx context.Context, subjectURI string, request *pb.RegisterInstanceRequest) error
	HeartbeatInstance(ctx context.Context, subjectURI string, request *pb.HeartbeatInstanceRequest) error
	GetInstance(ctx context.Context, subjectURI string, request *pb.GetInstanceRequest) (*pb.GetInstanceResponse, error)
	ListInstances(ctx context.Context, subjectURI string, request *pb.ListInstancesRequest) (*pb.ListInstancesResponse, error)
	RemoveInstance(ctx context.Context, subjectURI string, request *pb.RemoveInstanceRequest) error
	AcquireLeader(ctx context.Context, subjectURI string, request *pb.AcquireLeaderRequest) (*pb.AcquireLeaderResponse, error)
	RenewLeader(ctx context.Context, subjectURI string, request *pb.RenewLeaderRequest) (*pb.RenewLeaderResponse, error)
	ReleaseLeader(ctx context.Context, subjectURI string, request *pb.ReleaseLeaderRequest) error
	GetLeader(ctx context.Context, subjectURI string, request *pb.GetLeaderRequest) (*pb.GetLeaderResponse, error)
	ResolveEndpoint(ctx context.Context, subjectURI string, request *pb.ResolveEndpointRequest) (*pb.ResolveEndpointResponse, error)
	ListParticipants(ctx context.Context, subjectURI string, request *pb.ListParticipantsRequest) (*pb.ListParticipantsResponse, error)
	UpsertRequest(ctx context.Context, subjectURI string, request *pb.UpsertRequestRequest) error
	GetRequest(ctx context.Context, subjectURI string, request *pb.GetRequestRequest) (*pb.GetRequestResponse, error)
	ListRequests(ctx context.Context, subjectURI string, request *pb.ListRequestsRequest) (*pb.ListRequestsResponse, error)
	UpsertTask(ctx context.Context, subjectURI string, request *pb.UpsertTaskRequest) error
	ListTasks(ctx context.Context, subjectURI string, request *pb.ListTasksRequest) (*pb.ListTasksResponse, error)
	AcquireProfileLease(ctx context.Context, subjectURI string, request *pb.AcquireProfileLeaseRequest) (*pb.AcquireProfileLeaseResponse, error)
	RenewProfileLease(ctx context.Context, subjectURI string, request *pb.RenewProfileLeaseRequest) (*pb.RenewProfileLeaseResponse, error)
	GetProfileLease(ctx context.Context, subjectURI string, request *pb.GetProfileLeaseRequest) (*pb.GetProfileLeaseResponse, error)
	ReleaseProfileLease(ctx context.Context, subjectURI string, request *pb.ReleaseProfileLeaseRequest) error
	AcquireTaskLease(ctx context.Context, subjectURI string, request *pb.AcquireTaskLeaseRequest) (*pb.AcquireTaskLeaseResponse, error)
	RenewTaskLease(ctx context.Context, subjectURI string, request *pb.RenewTaskLeaseRequest) (*pb.RenewTaskLeaseResponse, error)
	GetTaskLease(ctx context.Context, subjectURI string, request *pb.GetTaskLeaseRequest) (*pb.GetTaskLeaseResponse, error)
	ReleaseTaskLease(ctx context.Context, subjectURI string, request *pb.ReleaseTaskLeaseRequest) error
}

// NewServer creates a gRPC server bound to listenAddr (use ":0" for dynamic port).
func NewServer(name, listenAddr string, bufferSize int, tlsConfig ...*tls.Config) (*Server, error) {
	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", listenAddr, err)
	}

	var opts []grpc.ServerOption
	if len(tlsConfig) > 0 && tlsConfig[0] != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(tlsConfig[0])))
	}

	s := &Server{
		name:       name,
		inbox:      make(chan *message.Message, bufferSize),
		grpcServer: grpc.NewServer(opts...),
		listener:   lis,
	}
	s.status.Store("ready")
	pb.RegisterAgentServiceServer(s.grpcServer, s)
	pb.RegisterControlPlaneServiceServer(s.grpcServer, s)
	return s, nil
}

func (s *Server) SendMessage(ctx context.Context, pbMsg *pb.Message) (*pb.SendMessageResponse, error) {
	msg := message.FromProto(pbMsg)
	if msg == nil {
		return &pb.SendMessageResponse{Accepted: false, Error: "nil message"}, nil
	}
	if msg.From == "" {
		return &pb.SendMessageResponse{Accepted: false, Error: "missing sender"}, nil
	}
	if msg.To != s.name {
		return &pb.SendMessageResponse{Accepted: false, Error: fmt.Sprintf("wrong recipient %q", msg.To)}, nil
	}
	if err := verifyPeerIdentity(ctx, msg.From, msg.Metadata); err != nil {
		return &pb.SendMessageResponse{Accepted: false, Error: err.Error()}, nil
	}
	select {
	case s.inbox <- msg:
		return &pb.SendMessageResponse{Accepted: true}, nil
	default:
		return &pb.SendMessageResponse{Accepted: false, Error: fmt.Sprintf("inbox full for %s", s.name)}, nil
	}
}

func verifyPeerIdentity(ctx context.Context, claimedSender string, metadata map[string]string) error {
	p, ok := peer.FromContext(ctx)
	if !ok || p.AuthInfo == nil {
		return nil
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return nil
	}
	if len(tlsInfo.State.PeerCertificates) == 0 {
		return fmt.Errorf("missing peer certificate")
	}

	subject, err := identity.SubjectFromCertificate(tlsInfo.State.PeerCertificates[0])
	if err != nil {
		certCN := tlsInfo.State.PeerCertificates[0].Subject.CommonName
		if certCN == "" {
			return fmt.Errorf("peer certificate missing common name")
		}
		if certCN != claimedSender {
			return fmt.Errorf("sender mismatch: claimed %q, cert %q", claimedSender, certCN)
		}
		return nil
	}

	switch subject.Kind {
	case identity.SubjectKindConductor:
		if identity.NormalizeID(claimedSender) != subject.Name {
			return fmt.Errorf("sender mismatch: claimed %q, subject %q", claimedSender, subject.URI())
		}
		return nil
	case identity.SubjectKindAgentProfile:
		if identity.NormalizeID(claimedSender) != subject.Profile {
			return fmt.Errorf("sender mismatch: claimed %q, subject %q", claimedSender, subject.URI())
		}
		return nil
	case identity.SubjectKindAgentInstance:
		if identity.NormalizeID(claimedSender) != subject.Profile {
			return fmt.Errorf("sender mismatch: claimed %q, subject %q", claimedSender, subject.URI())
		}
		return nil
	case identity.SubjectKindNode:
		return authorizeNodeBackedSender(subject, claimedSender, metadata)
	default:
		return fmt.Errorf("subject %q is not authorized for agent messaging", subject.Kind)
	}
}

func authorizeNodeBackedSender(subject identity.Subject, claimedSender string, metadata map[string]string) error {
	if identity.NormalizeID(metadata["sender_node"]) != subject.NodeID {
		return fmt.Errorf("node sender mismatch: subject node %q cannot act as node %q", subject.NodeID, metadata["sender_node"])
	}

	senderInstance := metadata["sender_instance"]
	if senderInstance == "" {
		if identity.NormalizeID(claimedSender) == subject.NodeID {
			return nil
		}
		return fmt.Errorf("node subject %q requires sender_instance metadata for claimed sender %q", subject.NodeID, claimedSender)
	}

	instanceNode, instanceProfile, ok := senderInstanceParts(senderInstance)
	if !ok {
		return fmt.Errorf("invalid sender instance %q", senderInstance)
	}
	if identity.NormalizeID(instanceNode) != subject.NodeID {
		return fmt.Errorf("node subject %q cannot act on instance %q", subject.NodeID, senderInstance)
	}
	if identity.NormalizeID(claimedSender) != identity.NormalizeID(instanceProfile) {
		return fmt.Errorf("node subject %q cannot claim sender %q for instance %q", subject.NodeID, claimedSender, senderInstance)
	}
	return nil
}

func senderInstanceParts(instanceID string) (nodeID, profile string, ok bool) {
	firstSlash := -1
	for index, ch := range instanceID {
		if ch == '/' {
			firstSlash = index
			break
		}
	}
	if firstSlash <= 0 || firstSlash >= len(instanceID)-1 {
		return "", "", false
	}

	rest := instanceID[firstSlash+1:]
	secondSlash := -1
	for index, ch := range rest {
		if ch == '/' {
			secondSlash = index
			break
		}
	}
	if secondSlash == -1 {
		return instanceID[:firstSlash], rest, true
	}
	if secondSlash == 0 {
		return "", "", false
	}
	return instanceID[:firstSlash], rest[:secondSlash], true
}

func (s *Server) Heartbeat(_ context.Context, _ *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	st, _ := s.status.Load().(string)
	return &pb.HeartbeatResponse{Name: s.name, Status: st}, nil
}

func (s *Server) RegisterNode(ctx context.Context, request *pb.RegisterNodeRequest) (*emptypb.Empty, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.controlPlane.RegisterNode(ctx, subjectURI, request); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) AttestNode(ctx context.Context, request *pb.AttestNodeRequest) (*pb.AttestNodeResponse, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	return s.controlPlane.AttestNode(ctx, subjectURI, request)
}

func (s *Server) HeartbeatNode(ctx context.Context, request *pb.HeartbeatNodeRequest) (*emptypb.Empty, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.controlPlane.HeartbeatNode(ctx, subjectURI, request); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) GetNode(ctx context.Context, request *pb.GetNodeRequest) (*pb.GetNodeResponse, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	return s.controlPlane.GetNode(ctx, subjectURI, request)
}

func (s *Server) ListNodes(ctx context.Context, request *pb.ListNodesRequest) (*pb.ListNodesResponse, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	return s.controlPlane.ListNodes(ctx, subjectURI, request)
}

func (s *Server) RegisterInstance(ctx context.Context, request *pb.RegisterInstanceRequest) (*emptypb.Empty, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.controlPlane.RegisterInstance(ctx, subjectURI, request); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) HeartbeatInstance(ctx context.Context, request *pb.HeartbeatInstanceRequest) (*emptypb.Empty, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.controlPlane.HeartbeatInstance(ctx, subjectURI, request); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) GetInstance(ctx context.Context, request *pb.GetInstanceRequest) (*pb.GetInstanceResponse, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	return s.controlPlane.GetInstance(ctx, subjectURI, request)
}

func (s *Server) ListInstances(ctx context.Context, request *pb.ListInstancesRequest) (*pb.ListInstancesResponse, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	return s.controlPlane.ListInstances(ctx, subjectURI, request)
}

func (s *Server) RemoveInstance(ctx context.Context, request *pb.RemoveInstanceRequest) (*emptypb.Empty, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.controlPlane.RemoveInstance(ctx, subjectURI, request); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) AcquireLeader(ctx context.Context, request *pb.AcquireLeaderRequest) (*pb.AcquireLeaderResponse, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	return s.controlPlane.AcquireLeader(ctx, subjectURI, request)
}

func (s *Server) RenewLeader(ctx context.Context, request *pb.RenewLeaderRequest) (*pb.RenewLeaderResponse, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	return s.controlPlane.RenewLeader(ctx, subjectURI, request)
}

func (s *Server) ReleaseLeader(ctx context.Context, request *pb.ReleaseLeaderRequest) (*emptypb.Empty, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.controlPlane.ReleaseLeader(ctx, subjectURI, request); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) GetLeader(ctx context.Context, request *pb.GetLeaderRequest) (*pb.GetLeaderResponse, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	return s.controlPlane.GetLeader(ctx, subjectURI, request)
}

func (s *Server) ResolveEndpoint(ctx context.Context, request *pb.ResolveEndpointRequest) (*pb.ResolveEndpointResponse, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	return s.controlPlane.ResolveEndpoint(ctx, subjectURI, request)
}

func (s *Server) ListParticipants(ctx context.Context, request *pb.ListParticipantsRequest) (*pb.ListParticipantsResponse, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	return s.controlPlane.ListParticipants(ctx, subjectURI, request)
}

func (s *Server) UpsertRequest(ctx context.Context, request *pb.UpsertRequestRequest) (*emptypb.Empty, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.controlPlane.UpsertRequest(ctx, subjectURI, request); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) GetRequest(ctx context.Context, request *pb.GetRequestRequest) (*pb.GetRequestResponse, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	return s.controlPlane.GetRequest(ctx, subjectURI, request)
}

func (s *Server) ListRequests(ctx context.Context, request *pb.ListRequestsRequest) (*pb.ListRequestsResponse, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	return s.controlPlane.ListRequests(ctx, subjectURI, request)
}

func (s *Server) UpsertTask(ctx context.Context, request *pb.UpsertTaskRequest) (*emptypb.Empty, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.controlPlane.UpsertTask(ctx, subjectURI, request); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) ListTasks(ctx context.Context, request *pb.ListTasksRequest) (*pb.ListTasksResponse, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	return s.controlPlane.ListTasks(ctx, subjectURI, request)
}

func (s *Server) AcquireProfileLease(ctx context.Context, request *pb.AcquireProfileLeaseRequest) (*pb.AcquireProfileLeaseResponse, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	return s.controlPlane.AcquireProfileLease(ctx, subjectURI, request)
}

func (s *Server) RenewProfileLease(ctx context.Context, request *pb.RenewProfileLeaseRequest) (*pb.RenewProfileLeaseResponse, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	return s.controlPlane.RenewProfileLease(ctx, subjectURI, request)
}

func (s *Server) GetProfileLease(ctx context.Context, request *pb.GetProfileLeaseRequest) (*pb.GetProfileLeaseResponse, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	return s.controlPlane.GetProfileLease(ctx, subjectURI, request)
}

func (s *Server) ReleaseProfileLease(ctx context.Context, request *pb.ReleaseProfileLeaseRequest) (*emptypb.Empty, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.controlPlane.ReleaseProfileLease(ctx, subjectURI, request); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) AcquireTaskLease(ctx context.Context, request *pb.AcquireTaskLeaseRequest) (*pb.AcquireTaskLeaseResponse, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	return s.controlPlane.AcquireTaskLease(ctx, subjectURI, request)
}

func (s *Server) RenewTaskLease(ctx context.Context, request *pb.RenewTaskLeaseRequest) (*pb.RenewTaskLeaseResponse, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	return s.controlPlane.RenewTaskLease(ctx, subjectURI, request)
}

func (s *Server) GetTaskLease(ctx context.Context, request *pb.GetTaskLeaseRequest) (*pb.GetTaskLeaseResponse, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	return s.controlPlane.GetTaskLease(ctx, subjectURI, request)
}

func (s *Server) ReleaseTaskLease(ctx context.Context, request *pb.ReleaseTaskLeaseRequest) (*emptypb.Empty, error) {
	if s.controlPlane == nil {
		return nil, fmt.Errorf("control-plane service not configured")
	}
	subjectURI, err := peerSubjectURI(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.controlPlane.ReleaseTaskLease(ctx, subjectURI, request); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) Serve() error {
	return s.grpcServer.Serve(s.listener)
}

func (s *Server) Stop() {
	s.status.Store("shutting_down")
	s.grpcServer.GracefulStop()
}

func (s *Server) Addr() string {
	return s.listener.Addr().String()
}

func (s *Server) Inbox() <-chan *message.Message {
	return s.inbox
}

func (s *Server) SetStatus(status string) {
	s.status.Store(status)
}

func (s *Server) SetControlPlaneService(service ControlPlaneService) {
	s.controlPlane = service
}

func peerSubjectURI(ctx context.Context) (string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok || p.AuthInfo == nil {
		return "", fmt.Errorf("missing peer auth info")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", fmt.Errorf("missing TLS peer auth info")
	}
	if len(tlsInfo.State.PeerCertificates) == 0 {
		return "", fmt.Errorf("missing peer certificate")
	}
	if len(tlsInfo.State.PeerCertificates[0].URIs) == 0 {
		return "", fmt.Errorf("peer certificate missing identity URI")
	}
	return tlsInfo.State.PeerCertificates[0].URIs[0].String(), nil
}
