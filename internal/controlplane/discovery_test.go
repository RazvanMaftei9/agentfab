package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

func TestDiscoveryResolvesConductorFromLeaderLease(t *testing.T) {
	store := NewMemoryStore("test-fabric")
	ctx := context.Background()

	if _, acquired, err := store.AcquireLeader(ctx, "conductor-a", "10.0.0.1:50050", 5*time.Second); err != nil {
		t.Fatalf("AcquireLeader: %v", err)
	} else if !acquired {
		t.Fatal("expected leader acquisition to succeed")
	}

	discovery := NewDiscovery(store)
	endpoint, err := discovery.Resolve(ctx, "conductor")
	if err != nil {
		t.Fatalf("Resolve conductor: %v", err)
	}
	if endpoint.Address != "10.0.0.1:50050" {
		t.Fatalf("endpoint address = %q, want 10.0.0.1:50050", endpoint.Address)
	}
}

func TestDiscoveryResolvesBestInstanceForProfile(t *testing.T) {
	store := NewMemoryStore("test-fabric")
	ctx := context.Background()

	if err := store.RegisterInstance(ctx, AgentInstance{
		ID:              "node-a/developer/1",
		Profile:         "developer",
		NodeID:          "node-a",
		Endpoint:        runtime.Endpoint{Address: "10.0.0.2:6001"},
		State:           InstanceStateBusy,
		LastHeartbeatAt: time.Now().Add(-2 * time.Second),
	}); err != nil {
		t.Fatalf("RegisterInstance busy: %v", err)
	}
	if err := store.RegisterInstance(ctx, AgentInstance{
		ID:              "node-b/developer/1",
		Profile:         "developer",
		NodeID:          "node-b",
		Endpoint:        runtime.Endpoint{Address: "10.0.0.3:6001"},
		State:           InstanceStateReady,
		LastHeartbeatAt: time.Now(),
	}); err != nil {
		t.Fatalf("RegisterInstance ready: %v", err)
	}

	discovery := NewDiscovery(store)
	endpoint, err := discovery.Resolve(ctx, "developer")
	if err != nil {
		t.Fatalf("Resolve developer: %v", err)
	}
	if endpoint.Address != "10.0.0.3:6001" {
		t.Fatalf("endpoint address = %q, want 10.0.0.3:6001", endpoint.Address)
	}
}

func TestDiscoveryOverrideTakesPrecedence(t *testing.T) {
	store := NewMemoryStore("test-fabric")
	ctx := context.Background()
	discovery := NewDiscovery(store)

	if err := discovery.Register(ctx, "architect", runtime.Endpoint{Address: "local"}); err != nil {
		t.Fatalf("Register override: %v", err)
	}

	endpoint, err := discovery.Resolve(ctx, "architect")
	if err != nil {
		t.Fatalf("Resolve architect: %v", err)
	}
	if endpoint.Address != "local" {
		t.Fatalf("endpoint address = %q, want local", endpoint.Address)
	}
}
