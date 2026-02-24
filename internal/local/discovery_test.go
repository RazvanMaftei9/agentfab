package local

import (
	"context"
	"testing"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

func TestDiscoveryRegisterResolve(t *testing.T) {
	d := NewDiscovery()
	ctx := context.Background()

	ep := runtime.Endpoint{Address: "local", Local: true}
	d.Register(ctx, "architect", ep)

	resolved, err := d.Resolve(ctx, "architect")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Address != "local" || !resolved.Local {
		t.Errorf("got %+v", resolved)
	}
}

func TestDiscoveryResolveNotFound(t *testing.T) {
	d := NewDiscovery()
	_, err := d.Resolve(context.Background(), "nobody")
	if err == nil {
		t.Fatal("expected not found error")
	}
}

func TestDiscoveryList(t *testing.T) {
	d := NewDiscovery()
	ctx := context.Background()

	d.Register(ctx, "a", runtime.Endpoint{})
	d.Register(ctx, "b", runtime.Endpoint{})

	names, _ := d.List(ctx)
	if len(names) != 2 {
		t.Errorf("expected 2, got %d", len(names))
	}
}

func TestDiscoveryDeregister(t *testing.T) {
	d := NewDiscovery()
	ctx := context.Background()

	d.Register(ctx, "agent", runtime.Endpoint{})
	d.Deregister(ctx, "agent")

	_, err := d.Resolve(ctx, "agent")
	if err == nil {
		t.Fatal("expected not found after deregister")
	}
}
