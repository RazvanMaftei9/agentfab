package grpc

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync/atomic"

	pb "github.com/razvanmaftei/agentfab/gen/agentfab/v1"
	"github.com/razvanmaftei/agentfab/internal/message"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// Server receives messages for a single participant via gRPC.
type Server struct {
	pb.UnimplementedAgentServiceServer

	name       string
	inbox      chan *message.Message
	grpcServer *grpc.Server
	listener   net.Listener
	status     atomic.Value // string: "ready", "busy", "shutting_down"
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
	if err := verifyPeerIdentity(ctx, msg.From); err != nil {
		return &pb.SendMessageResponse{Accepted: false, Error: err.Error()}, nil
	}
	select {
	case s.inbox <- msg:
		return &pb.SendMessageResponse{Accepted: true}, nil
	default:
		return &pb.SendMessageResponse{Accepted: false, Error: fmt.Sprintf("inbox full for %s", s.name)}, nil
	}
}

func verifyPeerIdentity(ctx context.Context, claimedSender string) error {
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
	certCN := tlsInfo.State.PeerCertificates[0].Subject.CommonName
	if certCN == "" {
		return fmt.Errorf("peer certificate missing common name")
	}
	if certCN != claimedSender {
		return fmt.Errorf("sender mismatch: claimed %q, cert %q", claimedSender, certCN)
	}
	return nil
}

func (s *Server) Heartbeat(_ context.Context, _ *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	st, _ := s.status.Load().(string)
	return &pb.HeartbeatResponse{Name: s.name, Status: st}, nil
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
