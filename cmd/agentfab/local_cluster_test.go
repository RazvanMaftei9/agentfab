package main

import (
	"context"
	"testing"

	"github.com/razvanmaftei/agentfab/internal/config"
	"github.com/razvanmaftei/agentfab/internal/controlplane"
)

func TestResolveBootstrapNodeCount(t *testing.T) {
	systemDef := config.DefaultFabricDef("test-fabric")

	count, err := resolveBootstrapNodeCount(systemDef, true, "", 0, false)
	if err != nil {
		t.Fatalf("resolveBootstrapNodeCount auto: %v", err)
	}
	if count != 1 {
		t.Fatalf("auto bootstrap count = %d, want 1", count)
	}

	count, err = resolveBootstrapNodeCount(systemDef, true, "", 2, true)
	if err != nil {
		t.Fatalf("resolveBootstrapNodeCount explicit: %v", err)
	}
	if count != 2 {
		t.Fatalf("explicit bootstrap count = %d, want 2", count)
	}

	systemDef.ControlPlane.API.Address = "127.0.0.1:50051"
	count, err = resolveBootstrapNodeCount(systemDef, true, "", 0, false)
	if err != nil {
		t.Fatalf("resolveBootstrapNodeCount configured API: %v", err)
	}
	if count != 0 {
		t.Fatalf("configured API bootstrap count = %d, want 0", count)
	}

	if _, err := resolveBootstrapNodeCount(systemDef, false, "", 1, true); err == nil {
		t.Fatal("expected bootstrap count with non-external mode to fail")
	}
}

func TestLocalBootstrapListenAddr(t *testing.T) {
	if got := localBootstrapListenAddr(true, ":50050", false); got != "127.0.0.1:0" {
		t.Fatalf("localBootstrapListenAddr auto = %q, want 127.0.0.1:0", got)
	}
	if got := localBootstrapListenAddr(true, ":50050", true); got != ":50050" {
		t.Fatalf("localBootstrapListenAddr explicit = %q, want :50050", got)
	}
	if got := localBootstrapListenAddr(false, ":50050", false); got != ":50050" {
		t.Fatalf("localBootstrapListenAddr non-bootstrap = %q, want :50050", got)
	}
}

func TestStartLocalCluster(t *testing.T) {
	systemDef := config.DefaultFabricDef("test-fabric")
	dataDir := t.TempDir()

	cluster, err := startLocalCluster(context.Background(), systemDef, dataDir, config.BundleVerificationResult{}, 1, false, nil)
	if err != nil {
		t.Fatalf("startLocalCluster: %v", err)
	}
	defer cluster.Shutdown(context.Background())

	if cluster.controlPlaneAddress == "" {
		t.Fatal("expected control-plane address")
	}

	nodes, err := cluster.store.ListNodes(context.Background())
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("node count = %d, want 1", len(nodes))
	}

	instances, err := cluster.store.ListInstances(context.Background(), controlplaneFilterAll())
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(instances) == 0 {
		t.Fatal("expected at least one hosted instance")
	}
}

func controlplaneFilterAll() controlplane.InstanceFilter {
	return controlplane.InstanceFilter{}
}
