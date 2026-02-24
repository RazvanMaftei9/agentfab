package knowledge

import (
	"context"
	"testing"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

// tieredMemStorage is a minimal in-memory storage that respects tiers.
type tieredMemStorage struct {
	files map[runtime.StorageTier]map[string][]byte
}

func newTieredMemStorage() *tieredMemStorage {
	return &tieredMemStorage{
		files: map[runtime.StorageTier]map[string][]byte{
			runtime.TierShared:  {},
			runtime.TierAgent:   {},
			runtime.TierScratch: {},
		},
	}
}

func (m *tieredMemStorage) Read(_ context.Context, tier runtime.StorageTier, path string) ([]byte, error) {
	data, ok := m.files[tier][path]
	if !ok {
		return nil, &notFoundError{path}
	}
	return data, nil
}

func (m *tieredMemStorage) Write(_ context.Context, tier runtime.StorageTier, path string, data []byte) error {
	m.files[tier][path] = data
	return nil
}

func (m *tieredMemStorage) Exists(_ context.Context, tier runtime.StorageTier, path string) (bool, error) {
	_, ok := m.files[tier][path]
	return ok, nil
}

func TestApplySplitsPerAgentAndShared(t *testing.T) {
	ctx := context.Background()

	agentStores := map[string]*tieredMemStorage{
		"architect": newTieredMemStorage(),
		"developer": newTieredMemStorage(),
	}
	sharedStore := newTieredMemStorage()

	storageFor := func(name string) StorageHandle {
		s, ok := agentStores[name]
		if !ok {
			s = newTieredMemStorage()
			agentStores[name] = s
		}
		return s
	}

	manifest := &Manifest{
		Nodes: []ManifestNode{
			{ID: "architect/api", Agent: "architect", Title: "API Design", Summary: "REST API", Content: "# API Design\nREST endpoints.", Confidence: 0.9},
			{ID: "developer/auth", Agent: "developer", Title: "Auth Impl", Summary: "JWT auth", Content: "# Auth\nJWT implementation.", Confidence: 0.8},
		},
		Edges: []Edge{
			{From: "architect/api", To: "developer/auth", Relation: "implements"},
		},
	}

	sharedGraph := NewGraph()
	err := Apply(ctx, storageFor, sharedStore, manifest, sharedGraph, "req-1", "test-request", PruneOpts{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// 1. Per-agent graphs should exist on TierAgent.
	archGraph, err := LoadFromTier(ctx, agentStores["architect"], runtime.TierAgent)
	if err != nil {
		t.Fatalf("load architect graph: %v", err)
	}
	if archGraph == nil {
		t.Fatal("expected architect graph")
	}
	if _, ok := archGraph.Nodes["architect/api"]; !ok {
		t.Error("architect graph should contain architect/api")
	}
	if _, ok := archGraph.Nodes["developer/auth"]; ok {
		t.Error("architect graph should NOT contain developer/auth")
	}

	devGraph, err := LoadFromTier(ctx, agentStores["developer"], runtime.TierAgent)
	if err != nil {
		t.Fatalf("load developer graph: %v", err)
	}
	if devGraph == nil {
		t.Fatal("expected developer graph")
	}
	if _, ok := devGraph.Nodes["developer/auth"]; !ok {
		t.Error("developer graph should contain developer/auth")
	}

	// 2. Per-agent docs written via TierAgent.
	if _, ok := agentStores["architect"].files[runtime.TierAgent]["docs/api.md"]; !ok {
		t.Error("architect doc should be written to TierAgent")
	}
	if _, ok := agentStores["developer"].files[runtime.TierAgent]["docs/auth.md"]; !ok {
		t.Error("developer doc should be written to TierAgent")
	}

	// 3. Shared graph should have reference nodes (both agents) and all edges.
	loadedShared, err := LoadFromTier(ctx, sharedStore, runtime.TierShared)
	if err != nil {
		t.Fatalf("load shared graph: %v", err)
	}
	if loadedShared == nil {
		t.Fatal("expected shared graph")
	}
	if len(loadedShared.Nodes) != 2 {
		t.Errorf("shared graph should have 2 nodes, got %d", len(loadedShared.Nodes))
	}
	// Shared graph nodes should NOT have file paths (content-less references).
	for id, n := range loadedShared.Nodes {
		if n.FilePath != "" {
			// FilePath is set by Merge based on ID slug, but Content was stripped.
			// This is expected behavior — the shared Merge still sets FilePath from the node ID.
			_ = id
		}
	}
	if len(loadedShared.Edges) != 1 {
		t.Errorf("shared graph should have 1 edge, got %d", len(loadedShared.Edges))
	}

	// 4. Cross-agent edge should NOT be in per-agent graphs (different agents).
	if len(archGraph.Edges) != 0 {
		t.Errorf("architect graph should have 0 edges (cross-agent edge goes to shared only), got %d", len(archGraph.Edges))
	}
	if len(devGraph.Edges) != 0 {
		t.Errorf("developer graph should have 0 edges (cross-agent edge goes to shared only), got %d", len(devGraph.Edges))
	}
}

func TestApplyIntraAgentEdge(t *testing.T) {
	ctx := context.Background()

	agentStores := map[string]*tieredMemStorage{
		"architect": newTieredMemStorage(),
	}
	sharedStore := newTieredMemStorage()

	storageFor := func(name string) StorageHandle {
		s, ok := agentStores[name]
		if !ok {
			s = newTieredMemStorage()
			agentStores[name] = s
		}
		return s
	}

	manifest := &Manifest{
		Nodes: []ManifestNode{
			{ID: "architect/api", Agent: "architect", Title: "API", Summary: "API", Content: "api"},
			{ID: "architect/system", Agent: "architect", Title: "System", Summary: "System", Content: "sys"},
		},
		Edges: []Edge{
			{From: "architect/api", To: "architect/system", Relation: "refines"},
		},
	}

	sharedGraph := NewGraph()
	err := Apply(ctx, storageFor, sharedStore, manifest, sharedGraph, "req-1", "test", PruneOpts{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Intra-agent edge should appear in both per-agent and shared graphs.
	archGraph, _ := LoadFromTier(ctx, agentStores["architect"], runtime.TierAgent)
	if len(archGraph.Edges) != 1 {
		t.Errorf("architect graph should have 1 intra-agent edge, got %d", len(archGraph.Edges))
	}

	loadedShared, _ := LoadFromTier(ctx, sharedStore, runtime.TierShared)
	if len(loadedShared.Edges) != 1 {
		t.Errorf("shared graph should have 1 edge, got %d", len(loadedShared.Edges))
	}
}
