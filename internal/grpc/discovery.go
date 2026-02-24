package grpc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

// StaticDiscovery resolves agent names to gRPC addresses using an in-memory registry.
// In distributed mode, it falls back to a peers file for agents not registered locally.
type StaticDiscovery struct {
	mu        sync.RWMutex
	endpoints map[string]runtime.Endpoint
	peersFile string // optional path to peers.json for fallback resolution
}

func NewStaticDiscovery() *StaticDiscovery {
	return &StaticDiscovery{endpoints: make(map[string]runtime.Endpoint)}
}

// SetPeersFile sets the path to a peers.json file for fallback resolution.
// Agents spawned in distributed mode use this to discover peers whose
// addresses are assigned dynamically after they start.
func (d *StaticDiscovery) SetPeersFile(path string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.peersFile = path
}

func (d *StaticDiscovery) Register(_ context.Context, name string, endpoint runtime.Endpoint) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.endpoints[name] = endpoint
	return nil
}

func (d *StaticDiscovery) Resolve(_ context.Context, name string) (runtime.Endpoint, error) {
	d.mu.RLock()
	ep, ok := d.endpoints[name]
	pf := d.peersFile
	d.mu.RUnlock()

	if ok {
		return ep, nil
	}

	// Fallback: try loading from peers file (written by conductor after all
	// agents are spawned). This handles the case where dynamically-assigned
	// addresses aren't known at agent startup time.
	if pf != "" {
		if peer, err := d.resolveFromPeersFile(pf, name); err == nil {
			// Cache for future lookups.
			d.mu.Lock()
			d.endpoints[name] = peer
			d.mu.Unlock()
			return peer, nil
		}
	}

	return runtime.Endpoint{}, fmt.Errorf("agent %q not found in discovery", name)
}

func (d *StaticDiscovery) resolveFromPeersFile(path, name string) (runtime.Endpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return runtime.Endpoint{}, err
	}
	var peers map[string]string // name → address
	if err := json.Unmarshal(data, &peers); err != nil {
		return runtime.Endpoint{}, err
	}
	addr, ok := peers[name]
	if !ok {
		return runtime.Endpoint{}, fmt.Errorf("agent %q not in peers file", name)
	}
	return runtime.Endpoint{Address: addr}, nil
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
