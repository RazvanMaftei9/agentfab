package controlplane

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

type Discovery struct {
	store     Store
	mu        sync.RWMutex
	overrides map[string]runtime.Endpoint
}

func NewDiscovery(store Store) *Discovery {
	return &Discovery{
		store:     store,
		overrides: make(map[string]runtime.Endpoint),
	}
}

func (d *Discovery) Register(_ context.Context, name string, endpoint runtime.Endpoint) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.overrides[name] = endpoint
	return nil
}

func (d *Discovery) Resolve(ctx context.Context, name string) (runtime.Endpoint, error) {
	d.mu.RLock()
	if endpoint, ok := d.overrides[name]; ok {
		d.mu.RUnlock()
		return endpoint, nil
	}
	d.mu.RUnlock()

	endpoint, ok, err := ResolveEndpoint(ctx, d.store, name)
	if err != nil {
		return runtime.Endpoint{}, err
	}
	if !ok {
		return runtime.Endpoint{}, fmt.Errorf("agent %q not found in control plane discovery", name)
	}
	return endpoint, nil
}

func (d *Discovery) List(ctx context.Context) ([]string, error) {
	d.mu.RLock()
	names := make([]string, 0, len(d.overrides))
	seen := make(map[string]bool, len(d.overrides))
	for name := range d.overrides {
		names = append(names, name)
		seen[name] = true
	}
	d.mu.RUnlock()

	discovered, err := ListParticipants(ctx, d.store)
	if err != nil {
		return nil, err
	}
	for _, name := range discovered {
		if seen[name] {
			continue
		}
		names = append(names, name)
		seen[name] = true
	}

	sort.Strings(names)
	return names, nil
}

func (d *Discovery) Deregister(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.overrides, name)
	return nil
}

func selectInstance(instances []AgentInstance) (AgentInstance, bool) {
	sort.Slice(instances, func(i, j int) bool {
		left := instancePriority(instances[i].State)
		right := instancePriority(instances[j].State)
		if left != right {
			return left < right
		}
		if !instances[i].LastHeartbeatAt.Equal(instances[j].LastHeartbeatAt) {
			return instances[i].LastHeartbeatAt.After(instances[j].LastHeartbeatAt)
		}
		return instances[i].ID < instances[j].ID
	})

	for _, instance := range instances {
		if instance.Endpoint.Address == "" {
			continue
		}
		if !isRoutableInstanceState(instance.State) {
			continue
		}
		return instance, true
	}
	return AgentInstance{}, false
}

func ResolveEndpoint(ctx context.Context, store Store, name string) (runtime.Endpoint, bool, error) {
	if name == "conductor" {
		leader, ok, err := store.GetLeader(ctx)
		if err != nil {
			return runtime.Endpoint{}, false, err
		}
		if !ok || leader.HolderAddress == "" {
			return runtime.Endpoint{}, false, nil
		}
		return runtime.Endpoint{Address: leader.HolderAddress}, true, nil
	}

	instance, ok, err := store.GetInstance(ctx, name)
	if err != nil {
		return runtime.Endpoint{}, false, err
	}
	if ok && instance.Endpoint.Address != "" && isRoutableInstanceState(instance.State) {
		return instance.Endpoint, true, nil
	}

	instances, err := store.ListInstances(ctx, InstanceFilter{Profile: name})
	if err != nil {
		return runtime.Endpoint{}, false, err
	}

	best, ok := selectInstance(instances)
	if !ok {
		return runtime.Endpoint{}, false, nil
	}
	return best.Endpoint, true, nil
}

func ListParticipants(ctx context.Context, store Store) ([]string, error) {
	instances, err := store.ListInstances(ctx, InstanceFilter{})
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(instances)*2+1)
	seen := make(map[string]bool, len(instances)*2+1)
	for _, instance := range instances {
		if instance.ID != "" && !seen[instance.ID] {
			names = append(names, instance.ID)
			seen[instance.ID] = true
		}
		if instance.Profile != "" && !seen[instance.Profile] {
			names = append(names, instance.Profile)
			seen[instance.Profile] = true
		}
	}

	if leader, ok, err := store.GetLeader(ctx); err == nil && ok && leader.HolderAddress != "" && !seen["conductor"] {
		names = append(names, "conductor")
	}

	sort.Strings(names)
	return names, nil
}

func isRoutableInstanceState(state InstanceState) bool {
	switch state {
	case InstanceStateReady, InstanceStateBusy:
		return true
	default:
		return false
	}
}

func instancePriority(state InstanceState) int {
	switch state {
	case InstanceStateReady:
		return 0
	case InstanceStateBusy:
		return 1
	case InstanceStateStarting:
		return 2
	default:
		return 3
	}
}
