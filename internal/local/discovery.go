package local

import (
	"context"
	"fmt"
	"sync"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

type Discovery struct {
	mu        sync.RWMutex
	endpoints map[string]runtime.Endpoint
}

func NewDiscovery() *Discovery {
	return &Discovery{endpoints: make(map[string]runtime.Endpoint)}
}

func (d *Discovery) Register(_ context.Context, name string, endpoint runtime.Endpoint) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.endpoints[name] = endpoint
	return nil
}

func (d *Discovery) Resolve(_ context.Context, name string) (runtime.Endpoint, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	ep, ok := d.endpoints[name]
	if !ok {
		return runtime.Endpoint{}, fmt.Errorf("agent %q not found", name)
	}
	return ep, nil
}

func (d *Discovery) List(_ context.Context) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	names := make([]string, 0, len(d.endpoints))
	for name := range d.endpoints {
		names = append(names, name)
	}
	return names, nil
}

func (d *Discovery) Deregister(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.endpoints, name)
	return nil
}
