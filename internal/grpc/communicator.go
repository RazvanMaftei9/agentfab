package grpc

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"

	pb "github.com/razvanmaftei/agentfab/gen/agentfab/v1"
	"github.com/razvanmaftei/agentfab/internal/message"
	"github.com/razvanmaftei/agentfab/internal/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

var _ message.MessageCommunicator = (*Communicator)(nil)

// Communicator implements message.MessageCommunicator over gRPC.
type Communicator struct {
	name      string
	server    *Server           // local gRPC server (inbox source)
	discovery runtime.Discovery // resolves agent names → addresses
	tlsConfig *tls.Config       // optional TLS config for client connections
	mu        sync.RWMutex
	conns     map[string]*grpc.ClientConn
}

func NewCommunicator(name string, server *Server, discovery runtime.Discovery, tlsConfig ...*tls.Config) *Communicator {
	c := &Communicator{
		name:      name,
		server:    server,
		discovery: discovery,
		conns:     make(map[string]*grpc.ClientConn),
	}
	if len(tlsConfig) > 0 {
		c.tlsConfig = tlsConfig[0]
	}
	return c
}

func (c *Communicator) Send(ctx context.Context, msg *message.Message) error {
	target := msg.To
	client, err := c.getClient(ctx, target)
	if err != nil {
		return fmt.Errorf("connect to %q: %w", target, err)
	}
	pbMsg := message.ToProto(msg)
	resp, err := client.SendMessage(ctx, pbMsg)
	if err != nil {
		return fmt.Errorf("send to %q: %w", target, err)
	}
	if !resp.Accepted {
		return fmt.Errorf("send to %q rejected: %s", target, resp.Error)
	}
	return nil
}

func (c *Communicator) Receive(_ context.Context) <-chan *message.Message {
	return c.server.Inbox()
}

func (c *Communicator) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, conn := range c.conns {
		conn.Close()
	}
	c.conns = make(map[string]*grpc.ClientConn)
}

func (c *Communicator) getClient(ctx context.Context, target string) (pb.AgentServiceClient, error) {
	c.mu.RLock()
	conn, ok := c.conns[target]
	c.mu.RUnlock()
	if ok {
		return pb.NewAgentServiceClient(conn), nil
	}

	ep, err := c.discovery.Resolve(ctx, target)
	if err != nil {
		return nil, err
	}

	var creds grpc.DialOption
	if c.tlsConfig != nil {
		creds = grpc.WithTransportCredentials(credentials.NewTLS(c.tlsConfig))
	} else {
		creds = grpc.WithTransportCredentials(insecure.NewCredentials())
	}
	conn, err = grpc.NewClient(ep.Address, creds)
	if err != nil {
		return nil, fmt.Errorf("dial %q at %s: %w", target, ep.Address, err)
	}

	c.mu.Lock()
	// Double-check: another goroutine may have created the connection.
	if existing, ok := c.conns[target]; ok {
		conn.Close()
		conn = existing
	} else {
		c.conns[target] = conn
	}
	c.mu.Unlock()

	return pb.NewAgentServiceClient(conn), nil
}
