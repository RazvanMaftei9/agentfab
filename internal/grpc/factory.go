package grpc

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/razvanmaftei/agentfab/internal/message"
	"github.com/razvanmaftei/agentfab/internal/runtime"
)

var _ message.CommunicatorFactory = (*CommFactory)(nil)

// CommFactory creates gRPC-backed communicators with per-agent servers.
type CommFactory struct {
	mu        sync.Mutex
	discovery runtime.Discovery
	servers   map[string]*Server       // name → gRPC server
	comms     map[string]*Communicator // name → communicator
	serverTLS *tls.Config              // TLS config for servers
	clientTLS *tls.Config              // TLS config for client connections
}

func NewCommFactory(discovery runtime.Discovery, serverTLS, clientTLS *tls.Config) *CommFactory {
	return &CommFactory{
		discovery: discovery,
		servers:   make(map[string]*Server),
		comms:     make(map[string]*Communicator),
		serverTLS: serverTLS,
		clientTLS: clientTLS,
	}
}

func (f *CommFactory) Register(agentName string) message.MessageCommunicator {
	f.mu.Lock()
	defer f.mu.Unlock()

	if comm, ok := f.comms[agentName]; ok {
		return comm
	}

	srv, err := NewServer(agentName, ":0", 64, f.serverTLS)
	if err != nil {
		slog.Error("grpc factory: failed to create server", "agent", agentName, "error", err)
		return nil
	}

	// Use "localhost" for TLS so certs valid for "localhost" work.
	addr := srv.Addr()
	if f.serverTLS != nil {
		_, port, _ := net.SplitHostPort(addr)
		if port != "" {
			addr = net.JoinHostPort("localhost", port)
		}
	}
	f.discovery.Register(context.Background(), agentName, runtime.Endpoint{
		Address: addr,
		Local:   false,
	})

	go func() {
		if err := srv.Serve(); err != nil {
			slog.Debug("grpc server stopped", "agent", agentName, "error", err)
		}
	}()

	comm := NewCommunicator(agentName, srv, f.discovery, f.clientTLS)

	f.servers[agentName] = srv
	f.comms[agentName] = comm

	return comm
}

func (f *CommFactory) Deregister(agentName string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if srv, ok := f.servers[agentName]; ok {
		srv.Stop()
		delete(f.servers, agentName)
	}
	if comm, ok := f.comms[agentName]; ok {
		comm.Close()
		delete(f.comms, agentName)
	}
	f.discovery.Deregister(context.Background(), agentName)
}

func (f *CommFactory) StopAll() {
	f.mu.Lock()
	defer f.mu.Unlock()

	for name, srv := range f.servers {
		srv.Stop()
		delete(f.servers, name)
	}
	for name, comm := range f.comms {
		comm.Close()
		delete(f.comms, name)
	}
}

func (f *CommFactory) ServerAddr(agentName string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	srv, ok := f.servers[agentName]
	if !ok {
		return "", fmt.Errorf("agent %q not registered", agentName)
	}
	return srv.Addr(), nil
}
