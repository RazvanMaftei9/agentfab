package grpc

import (
	"context"
	"fmt"
	"sync"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

// StaticDiscovery resolves agent names to gRPC addresses using an in-memory
// registry.
type StaticDiscovery struct {
	mu        sync.RWMutex
	endpoints map[string]runtime.Endpoint
}

func NewStaticDiscovery() *StaticDiscovery {
	return &StaticDiscovery{endpoints: make(map[string]runtime.Endpoint)}
}

func (d *StaticDiscovery) Register(_ context.Context, name string, endpoint runtime.Endpoint) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.endpoints[name] = endpoint
	return nil
}

func (d *StaticDiscovery) Resolve(_ context.Context, name string) (runtime.Endpoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	ep, ok := d.endpoints[name]
	if !ok {
		return runtime.Endpoint{}, fmt.Errorf("agent %q not found in discovery", name)
	}
	return ep, nil
}

func (d *StaticDiscovery) List(_ context.Context) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	names := make([]string, 0, len(d.endpoints))
	for name := range d.endpoints {
		names = append(names, name)
	}
	return names, nil
}

func (d *StaticDiscovery) Deregister(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.endpoints, name)
	return nil
}
