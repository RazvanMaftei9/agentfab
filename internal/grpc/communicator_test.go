package grpc

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	pb "github.com/razvanmaftei/agentfab/gen/agentfab/v1"
	"github.com/razvanmaftei/agentfab/internal/message"
	"github.com/razvanmaftei/agentfab/internal/runtime"
	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestRoundTripMessage(t *testing.T) {
	discovery := NewStaticDiscovery()

	// Create two gRPC servers.
	srvA, err := NewServer("agent-a", ":0", 64)
	if err != nil {
		t.Fatalf("create server A: %v", err)
	}
	defer srvA.Stop()
	go srvA.Serve()

	srvB, err := NewServer("agent-b", ":0", 64)
	if err != nil {
		t.Fatalf("create server B: %v", err)
	}
	defer srvB.Stop()
	go srvB.Serve()

	// Register both in discovery.
	ctx := context.Background()
	discovery.Register(ctx, "agent-a", runtime.Endpoint{Address: srvA.Addr()})
	discovery.Register(ctx, "agent-b", runtime.Endpoint{Address: srvB.Addr()})

	// Create communicators.
	commA := NewCommunicator("agent-a", srvA, discovery)
	defer commA.Close()
	commB := NewCommunicator("agent-b", srvB, discovery)
	defer commB.Close()

	// Send message from A to B.
	msg := &message.Message{
		ID:        "msg-1",
		RequestID: "req-1",
		From:      "agent-a",
		To:        "agent-b",
		Type:      message.TypeTaskAssignment,
		Parts:     []message.Part{message.TextPart{Text: "hello via grpc"}},
		Timestamp: time.Now(),
	}

	if err := commA.Send(ctx, msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	// Receive at B.
	select {
	case received := <-commB.Receive(ctx):
		if received.ID != "msg-1" {
			t.Errorf("id: got %q, want msg-1", received.ID)
		}
		if received.From != "agent-a" {
			t.Errorf("from: got %q", received.From)
		}
		if received.Type != message.TypeTaskAssignment {
			t.Errorf("type: got %q", received.Type)
		}
		if len(received.Parts) == 0 {
			t.Fatal("no parts")
		}
		tp, ok := received.Parts[0].(message.TextPart)
		if !ok {
			t.Fatal("part is not TextPart")
		}
		if tp.Text != "hello via grpc" {
			t.Errorf("text: got %q", tp.Text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for message")
	}
}

func TestHeartbeat(t *testing.T) {
	srv, err := NewServer("test-agent", ":0", 8)
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	defer srv.Stop()
	go srv.Serve()

	// Connect as client.
	conn, err := grpclib.NewClient(srv.Addr(),
		grpclib.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	client := pb.NewAgentServiceClient(conn)
	resp, err := client.Heartbeat(context.Background(), &pb.HeartbeatRequest{})
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if resp.Name != "test-agent" {
		t.Errorf("name: got %q", resp.Name)
	}
	if resp.Status != "ready" {
		t.Errorf("status: got %q", resp.Status)
	}
}

func TestSendRejectsWrongRecipient(t *testing.T) {
	srv, err := NewServer("target", ":0", 8)
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	defer srv.Stop()
	go srv.Serve()

	conn, err := grpclib.NewClient(srv.Addr(),
		grpclib.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	client := pb.NewAgentServiceClient(conn)
	resp, err := client.SendMessage(context.Background(), &pb.Message{
		Id:   "m1",
		From: "sender",
		To:   "someone-else",
		Type: pb.MessageType_MESSAGE_TYPE_STATUS_UPDATE,
	})
	if err != nil {
		t.Fatalf("send message: %v", err)
	}
	if resp.Accepted {
		t.Fatal("expected rejection for wrong recipient")
	}
	if !strings.Contains(resp.Error, "wrong recipient") {
		t.Fatalf("expected wrong recipient error, got: %q", resp.Error)
	}
}

func TestSendToUnknownAgent(t *testing.T) {
	discovery := NewStaticDiscovery()

	srv, err := NewServer("sender", ":0", 8)
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	defer srv.Stop()
	go srv.Serve()

	comm := NewCommunicator("sender", srv, discovery)
	defer comm.Close()

	msg := &message.Message{
		From: "sender",
		To:   "nonexistent",
		Type: message.TypeTaskAssignment,
	}

	err = comm.Send(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error sending to unknown agent")
	}
}

func TestSendRoutesViaAssignedInstance(t *testing.T) {
	discovery := NewStaticDiscovery()

	sender, err := NewServer("conductor", ":0", 8)
	if err != nil {
		t.Fatalf("create sender server: %v", err)
	}
	defer sender.Stop()
	go sender.Serve()

	receiver, err := NewServer("developer", ":0", 8)
	if err != nil {
		t.Fatalf("create receiver server: %v", err)
	}
	defer receiver.Stop()
	go receiver.Serve()

	ctx := context.Background()
	discovery.Register(ctx, "node-a/developer/1", runtime.Endpoint{Address: receiver.Addr()})

	comm := NewCommunicator("conductor", sender, discovery)
	defer comm.Close()

	msg := &message.Message{
		ID:        "msg-instance",
		RequestID: "req-instance",
		From:      "conductor",
		To:        "developer",
		Type:      message.TypeTaskAssignment,
		Parts:     []message.Part{message.TextPart{Text: "route by assigned instance"}},
		Metadata: map[string]string{
			"assigned_instance": "node-a/developer/1",
		},
		Timestamp: time.Now(),
	}

	if err := comm.Send(ctx, msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	select {
	case received := <-receiver.Inbox():
		if received.To != "developer" {
			t.Fatalf("recipient = %q, want developer", received.To)
		}
		if received.Metadata["assigned_instance"] != "node-a/developer/1" {
			t.Fatalf("assigned instance metadata = %q, want node-a/developer/1", received.Metadata["assigned_instance"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for routed message")
	}
}

func TestSendToConductorIgnoresAssignedInstanceRouting(t *testing.T) {
	discovery := NewStaticDiscovery()

	sender, err := NewServer("developer", ":0", 8)
	if err != nil {
		t.Fatalf("create sender server: %v", err)
	}
	defer sender.Stop()
	go sender.Serve()

	conductor, err := NewServer("conductor", ":0", 8)
	if err != nil {
		t.Fatalf("create conductor server: %v", err)
	}
	defer conductor.Stop()
	go conductor.Serve()

	worker, err := NewServer("developer", ":0", 8)
	if err != nil {
		t.Fatalf("create worker server: %v", err)
	}
	defer worker.Stop()
	go worker.Serve()

	ctx := context.Background()
	discovery.Register(ctx, "conductor", runtime.Endpoint{Address: conductor.Addr()})
	discovery.Register(ctx, "node-a/developer/1", runtime.Endpoint{Address: worker.Addr()})

	comm := NewCommunicator("developer", sender, discovery)
	defer comm.Close()

	msg := &message.Message{
		ID:        "msg-conductor",
		RequestID: "req-conductor",
		From:      "developer",
		To:        "conductor",
		Type:      message.TypeTaskResult,
		Parts:     []message.Part{message.TextPart{Text: "loop completed"}},
		Metadata: map[string]string{
			"assigned_instance": "node-a/developer/1",
		},
		Timestamp: time.Now(),
	}

	if err := comm.Send(ctx, msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	select {
	case received := <-conductor.Inbox():
		if received.To != "conductor" {
			t.Fatalf("recipient = %q, want conductor", received.To)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for conductor-bound message")
	}

	select {
	case received := <-worker.Inbox():
		t.Fatalf("worker received misrouted message for %q", received.To)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestCommFactory(t *testing.T) {
	discovery := NewStaticDiscovery()
	factory := NewCommFactory(discovery, nil, nil)
	defer factory.StopAll()

	commA := factory.Register("agent-a")
	commB := factory.Register("agent-b")

	if commA == nil || commB == nil {
		t.Fatal("Register returned nil")
	}

	ctx := context.Background()

	// Send from A to B.
	msg := &message.Message{
		ID:        "test-1",
		From:      "agent-a",
		To:        "agent-b",
		Type:      message.TypeTaskResult,
		Parts:     []message.Part{message.TextPart{Text: "factory test"}},
		Timestamp: time.Now(),
	}

	if err := commA.Send(ctx, msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	select {
	case received := <-commB.Receive(ctx):
		if received.ID != "test-1" {
			t.Errorf("id: got %q", received.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}

	// Deregister B.
	factory.Deregister("agent-b")

	// Sending to B should now fail.
	msg.ID = "test-2"
	if err := commA.Send(ctx, msg); err == nil {
		t.Error("expected error after deregister")
	}
}

func TestTLSRoundTrip(t *testing.T) {
	// Create a cluster CA and issue unique per-participant certs.
	ca, err := NewClusterCA()
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}

	certA, err := ca.IssueCert("tls-a")
	if err != nil {
		t.Fatalf("issue cert A: %v", err)
	}
	certB, err := ca.IssueCert("tls-b")
	if err != nil {
		t.Fatalf("issue cert B: %v", err)
	}

	discovery := NewStaticDiscovery()

	// Create servers with their own certs, both trusting the same CA.
	srvA, err := NewServer("tls-a", ":0", 64, ServerTLSConfig(certA, ca.Pool()))
	if err != nil {
		t.Fatalf("server A: %v", err)
	}
	srvB, err := NewServer("tls-b", ":0", 64, ServerTLSConfig(certB, ca.Pool()))
	if err != nil {
		t.Fatalf("server B: %v", err)
	}
	go srvA.Serve()
	go srvB.Serve()
	defer srvA.Stop()
	defer srvB.Stop()

	discovery.Register(context.Background(), "tls-a", runtime.Endpoint{Address: "localhost:" + portOf(srvA)})
	discovery.Register(context.Background(), "tls-b", runtime.Endpoint{Address: "localhost:" + portOf(srvB)})

	// Create communicators with their own client certs.
	commA := NewCommunicator("tls-a", srvA, discovery, ClientTLSConfig(certA, ca.Pool()))
	commB := NewCommunicator("tls-b", srvB, discovery, ClientTLSConfig(certB, ca.Pool()))
	defer commA.Close()
	defer commB.Close()

	ctx := context.Background()
	msg := &message.Message{
		ID:        "tls-msg-1",
		From:      "tls-a",
		To:        "tls-b",
		Type:      message.TypeTaskAssignment,
		Parts:     []message.Part{message.TextPart{Text: "encrypted hello"}},
		Timestamp: time.Now(),
	}

	if err := commA.Send(ctx, msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	select {
	case received := <-commB.Receive(ctx):
		if received.ID != "tls-msg-1" {
			t.Errorf("id: got %q", received.ID)
		}
		tp, ok := received.Parts[0].(message.TextPart)
		if !ok || tp.Text != "encrypted hello" {
			t.Error("content mismatch")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

func TestTLSRejectsSpoofedSender(t *testing.T) {
	ca, err := NewClusterCA()
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}

	certA, err := ca.IssueCert("tls-a")
	if err != nil {
		t.Fatalf("issue cert A: %v", err)
	}
	certB, err := ca.IssueCert("tls-b")
	if err != nil {
		t.Fatalf("issue cert B: %v", err)
	}

	discovery := NewStaticDiscovery()

	srvA, err := NewServer("tls-a", ":0", 8, ServerTLSConfig(certA, ca.Pool()))
	if err != nil {
		t.Fatalf("server A: %v", err)
	}
	defer srvA.Stop()
	go srvA.Serve()

	srvB, err := NewServer("tls-b", ":0", 8, ServerTLSConfig(certB, ca.Pool()))
	if err != nil {
		t.Fatalf("server B: %v", err)
	}
	defer srvB.Stop()
	go srvB.Serve()

	discovery.Register(context.Background(), "tls-b", runtime.Endpoint{Address: "localhost:" + portOf(srvB)})

	commA := NewCommunicator("tls-a", srvA, discovery, ClientTLSConfig(certA, ca.Pool()))
	defer commA.Close()

	err = commA.Send(context.Background(), &message.Message{
		ID:        "spoofed-1",
		RequestID: "req-1",
		From:      "tls-b",
		To:        "tls-b",
		Type:      message.TypeStatusUpdate,
		Timestamp: time.Now(),
	})
	if err == nil {
		t.Fatal("expected spoofed sender to be rejected")
	}
	if !strings.Contains(err.Error(), "sender mismatch") {
		t.Fatalf("expected sender mismatch error, got: %v", err)
	}
}

func TestTLSAllowsNodeIdentityToRepresentHostedInstance(t *testing.T) {
	ca, err := NewClusterCA()
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}

	nodeCert, err := ca.IssueCertWithOptions("node-a", IssueCertOptions{
		IdentityURI: "spiffe://agentfab.local/fabric/test-fabric/node/node-a",
	})
	if err != nil {
		t.Fatalf("issue node cert: %v", err)
	}
	receiverCert, err := ca.IssueCertWithOptions("developer", IssueCertOptions{
		IdentityURI: "spiffe://agentfab.local/fabric/test-fabric/node/node-b/agent/developer/instance/node-b/developer",
	})
	if err != nil {
		t.Fatalf("issue receiver cert: %v", err)
	}

	discovery := NewStaticDiscovery()

	sender, err := NewServer("architect", ":0", 8, ServerTLSConfig(nodeCert, ca.Pool()))
	if err != nil {
		t.Fatalf("create sender server: %v", err)
	}
	defer sender.Stop()
	go sender.Serve()

	receiver, err := NewServer("developer", ":0", 8, ServerTLSConfig(receiverCert, ca.Pool()))
	if err != nil {
		t.Fatalf("create receiver server: %v", err)
	}
	defer receiver.Stop()
	go receiver.Serve()

	ctx := context.Background()
	discovery.Register(ctx, "node-b/developer/1", runtime.Endpoint{Address: "localhost:" + portOf(receiver)})

	comm := NewCommunicator("architect", sender, discovery, ClientTLSConfig(nodeCert, ca.Pool()))
	defer comm.Close()

	err = comm.Send(ctx, &message.Message{
		ID:   "node-backed-1",
		From: "architect",
		To:   "developer",
		Type: message.TypeTaskAssignment,
		Metadata: map[string]string{
			"assigned_instance": "node-b/developer/1",
			"sender_node":       "node-a",
			"sender_instance":   "node-a/architect/1",
		},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	select {
	case <-receiver.Inbox():
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for node-backed message")
	}
}

func TestTLSRejectsNodeIdentityWithoutMatchingSenderMetadata(t *testing.T) {
	ca, err := NewClusterCA()
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}

	nodeCert, err := ca.IssueCertWithOptions("node-a", IssueCertOptions{
		IdentityURI: "spiffe://agentfab.local/fabric/test-fabric/node/node-a",
	})
	if err != nil {
		t.Fatalf("issue node cert: %v", err)
	}
	receiverCert, err := ca.IssueCertWithOptions("developer", IssueCertOptions{
		IdentityURI: "spiffe://agentfab.local/fabric/test-fabric/node/node-b/agent/developer/instance/node-b/developer",
	})
	if err != nil {
		t.Fatalf("issue receiver cert: %v", err)
	}

	discovery := NewStaticDiscovery()

	sender, err := NewServer("architect", ":0", 8, ServerTLSConfig(nodeCert, ca.Pool()))
	if err != nil {
		t.Fatalf("create sender server: %v", err)
	}
	defer sender.Stop()
	go sender.Serve()

	receiver, err := NewServer("developer", ":0", 8, ServerTLSConfig(receiverCert, ca.Pool()))
	if err != nil {
		t.Fatalf("create receiver server: %v", err)
	}
	defer receiver.Stop()
	go receiver.Serve()

	ctx := context.Background()
	discovery.Register(ctx, "node-b/developer/1", runtime.Endpoint{Address: "localhost:" + portOf(receiver)})

	comm := NewCommunicator("architect", sender, discovery, ClientTLSConfig(nodeCert, ca.Pool()))
	defer comm.Close()

	err = comm.Send(ctx, &message.Message{
		ID:   "node-backed-bad-1",
		From: "architect",
		To:   "developer",
		Type: message.TypeTaskAssignment,
		Metadata: map[string]string{
			"assigned_instance": "node-b/developer/1",
			"sender_node":       "node-a",
			"sender_instance":   "node-a/designer/1",
		},
	})
	if err == nil {
		t.Fatal("expected node-backed spoofed sender to be rejected")
	}
}

func TestInboxFull(t *testing.T) {
	discovery := NewStaticDiscovery()

	// Create server with tiny buffer.
	srv, err := NewServer("target", ":0", 1)
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	defer srv.Stop()
	go srv.Serve()

	srvSender, err := NewServer("sender", ":0", 1)
	if err != nil {
		t.Fatalf("create sender server: %v", err)
	}
	defer srvSender.Stop()
	go srvSender.Serve()

	ctx := context.Background()
	discovery.Register(ctx, "target", runtime.Endpoint{Address: srv.Addr()})
	discovery.Register(ctx, "sender", runtime.Endpoint{Address: srvSender.Addr()})

	comm := NewCommunicator("sender", srvSender, discovery)
	defer comm.Close()

	// Fill the inbox.
	msg := &message.Message{From: "sender", To: "target", Type: message.TypeStatusUpdate}
	if err := comm.Send(ctx, msg); err != nil {
		t.Fatalf("first send: %v", err)
	}

	// Second send should fail (inbox full, buffer=1).
	err = comm.Send(ctx, msg)
	if err == nil {
		t.Error("expected error when inbox is full")
	}
}

// portOf extracts just the port from a server's Addr() (e.g. "[::]:12345" → "12345").
func portOf(srv *Server) string {
	_, port, _ := net.SplitHostPort(srv.Addr())
	return port
}
