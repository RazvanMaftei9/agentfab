package knowledge

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

// memStorage is a minimal in-memory storage for testing.
type memStorage struct {
	files map[string][]byte
}

func newMemStorage() *memStorage {
	return &memStorage{files: make(map[string][]byte)}
}

func (m *memStorage) Read(_ context.Context, _ runtime.StorageTier, path string) ([]byte, error) {
	data, ok := m.files[path]
	if !ok {
		return nil, &notFoundError{path}
	}
	return data, nil
}

func (m *memStorage) Write(_ context.Context, _ runtime.StorageTier, path string, data []byte) error {
	m.files[path] = data
	return nil
}

func (m *memStorage) Exists(_ context.Context, _ runtime.StorageTier, path string) (bool, error) {
	_, ok := m.files[path]
	return ok, nil
}

type notFoundError struct{ path string }

func (e *notFoundError) Error() string { return "not found: " + e.path }

func TestNewGraph(t *testing.T) {
	g := NewGraph()
	if g.Version != 1 {
		t.Errorf("version: got %d, want 1", g.Version)
	}
	if len(g.Nodes) != 0 {
		t.Errorf("nodes: got %d, want 0", len(g.Nodes))
	}
}

func TestMergeNewNodes(t *testing.T) {
	g := NewGraph()

	manifest := &Manifest{
		Nodes: []ManifestNode{
			{ID: "architect/api-design", Agent: "architect", Title: "API Design", Summary: "REST API design", Tags: []string{"api"}},
			{ID: "developer/auth-impl", Agent: "developer", Title: "Auth Implementation", Summary: "JWT auth", Tags: []string{"auth"}},
		},
		Edges: []Edge{
			{From: "architect/api-design", To: "developer/auth-impl", Relation: "implements"},
		},
	}

	g.Merge(manifest, "req-1", "todo-app-mvp")

	if len(g.Nodes) != 2 {
		t.Fatalf("nodes: got %d, want 2", len(g.Nodes))
	}
	if g.Nodes["architect/api-design"].Title != "API Design" {
		t.Errorf("title: got %q", g.Nodes["architect/api-design"].Title)
	}
	if g.Nodes["architect/api-design"].RequestName != "todo-app-mvp" {
		t.Errorf("request_name: got %q", g.Nodes["architect/api-design"].RequestName)
	}
	if g.Nodes["architect/api-design"].FilePath != "docs/api-design.md" {
		t.Errorf("file_path: got %q", g.Nodes["architect/api-design"].FilePath)
	}
	if len(g.Edges) != 1 {
		t.Fatalf("edges: got %d, want 1", len(g.Edges))
	}
	if g.Version != 2 {
		t.Errorf("version: got %d, want 2", g.Version)
	}
}

func TestMergeUpdatesExistingNode(t *testing.T) {
	g := NewGraph()

	// First merge.
	g.Merge(&Manifest{
		Nodes: []ManifestNode{
			{ID: "architect/api-design", Agent: "architect", Title: "API Design v1", Summary: "First version"},
		},
	}, "req-1", "first-request")

	created := g.Nodes["architect/api-design"].CreatedAt

	// Second merge updates the same node.
	g.Merge(&Manifest{
		Nodes: []ManifestNode{
			{ID: "architect/api-design", Agent: "architect", Title: "API Design v2", Summary: "Updated version"},
		},
	}, "req-2", "second-request")

	node := g.Nodes["architect/api-design"]
	if node.Title != "API Design v2" {
		t.Errorf("title not updated: got %q", node.Title)
	}
	if node.RequestName != "second-request" {
		t.Errorf("request_name not updated: got %q", node.RequestName)
	}
	if node.CreatedAt != created {
		t.Error("created_at was modified on update")
	}
	if len(g.Nodes) != 1 {
		t.Errorf("should still have 1 node, got %d", len(g.Nodes))
	}
}

func TestMergeDeduplicatesEdges(t *testing.T) {
	g := NewGraph()

	edge := Edge{From: "a/x", To: "b/y", Relation: "related_to"}

	g.Merge(&Manifest{Edges: []Edge{edge}}, "req-1", "first")
	g.Merge(&Manifest{Edges: []Edge{edge}}, "req-2", "second")

	if len(g.Edges) != 1 {
		t.Errorf("edges: got %d, want 1 (dedup failed)", len(g.Edges))
	}
}

func TestQuery(t *testing.T) {
	g := NewGraph()
	g.Merge(&Manifest{
		Nodes: []ManifestNode{
			{ID: "architect/a", Agent: "architect", Title: "A"},
			{ID: "developer/b", Agent: "developer", Title: "B"},
			{ID: "architect/c", Agent: "architect", Title: "C"},
		},
	}, "req-1", "test")

	nodes := g.Query([]string{"architect"})
	if len(nodes) != 2 {
		t.Fatalf("query architect: got %d, want 2", len(nodes))
	}

	nodes = g.Query([]string{"developer"})
	if len(nodes) != 1 {
		t.Fatalf("query developer: got %d, want 1", len(nodes))
	}

	nodes = g.Query([]string{"architect", "developer"})
	if len(nodes) != 3 {
		t.Fatalf("query both: got %d, want 3", len(nodes))
	}
}

func TestSummary(t *testing.T) {
	g := NewGraph()

	if s := g.Summary(); s != "" {
		t.Errorf("empty graph summary should be empty, got %q", s)
	}

	g.Merge(&Manifest{
		Nodes: []ManifestNode{
			{ID: "architect/api-design", Agent: "architect", Title: "API Design", Summary: "REST API with JWT auth", Tags: []string{"api", "rest"}},
		},
	}, "req-1", "todo-app-mvp")

	s := g.Summary()
	if s == "" {
		t.Fatal("expected non-empty summary")
	}
	if !containsStr(s, "architect/api-design") {
		t.Error("summary missing node ID")
	}
	if !containsStr(s, "API Design") {
		t.Error("summary missing title")
	}
	if !containsStr(s, "api, rest") {
		t.Error("summary missing tags")
	}
	if !containsStr(s, `from request: "todo-app-mvp"`) {
		t.Error("summary missing request provenance")
	}
	if !containsStr(s, "Summary: REST API with JWT auth") {
		t.Error("summary missing node summary text")
	}
}

func TestLoadSaveRoundTrip(t *testing.T) {
	store := newMemStorage()
	ctx := context.Background()

	g := NewGraph()
	g.Merge(&Manifest{
		Nodes: []ManifestNode{
			{ID: "architect/api-design", Agent: "architect", Title: "API Design", Summary: "REST API"},
		},
		Edges: []Edge{
			{From: "architect/api-design", To: "developer/impl", Relation: "implements"},
		},
	}, "req-1", "test")

	if err := Save(ctx, store, g); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := Load(ctx, store)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Version != g.Version {
		t.Errorf("version: got %d, want %d", loaded.Version, g.Version)
	}
	if len(loaded.Nodes) != 1 {
		t.Fatalf("nodes: got %d, want 1", len(loaded.Nodes))
	}
	if loaded.Nodes["architect/api-design"].Title != "API Design" {
		t.Errorf("title: got %q", loaded.Nodes["architect/api-design"].Title)
	}
	if len(loaded.Edges) != 1 {
		t.Errorf("edges: got %d, want 1", len(loaded.Edges))
	}

	// Verify it's valid JSON.
	var raw json.RawMessage
	if err := json.Unmarshal(store.files[graphPath], &raw); err != nil {
		t.Errorf("stored data is not valid JSON: %v", err)
	}
}

func TestLoadNonExistent(t *testing.T) {
	store := newMemStorage()
	g, err := Load(context.Background(), store)
	if err != nil {
		t.Fatalf("load non-existent: %v", err)
	}
	if g != nil {
		t.Error("expected nil graph for non-existent file")
	}
}

func TestMergeNewFieldsOnCreate(t *testing.T) {
	g := NewGraph()
	g.Merge(&Manifest{
		Nodes: []ManifestNode{
			{
				ID: "dev/auth", Agent: "developer", Title: "Auth",
				Summary: "Auth module", Confidence: 0.85, Source: "task_result", TTLDays: 30,
			},
		},
	}, "req-1", "test")

	node := g.Nodes["dev/auth"]
	if node.Confidence != 0.85 {
		t.Errorf("confidence: got %f, want 0.85", node.Confidence)
	}
	if node.Source != "task_result" {
		t.Errorf("source: got %q, want task_result", node.Source)
	}
	if node.TTLDays != 30 {
		t.Errorf("ttl_days: got %d, want 30", node.TTLDays)
	}
}

func TestMergeConfidencePreservation(t *testing.T) {
	g := NewGraph()

	// First merge with high confidence.
	g.Merge(&Manifest{
		Nodes: []ManifestNode{
			{ID: "dev/auth", Agent: "developer", Title: "Auth v1", Summary: "S", Confidence: 0.9, Source: "task_result"},
		},
	}, "req-1", "first")

	// Second merge with lower confidence — should NOT overwrite.
	g.Merge(&Manifest{
		Nodes: []ManifestNode{
			{ID: "dev/auth", Agent: "developer", Title: "Auth v2", Summary: "S2", Confidence: 0.5, Source: "inferred"},
		},
	}, "req-2", "second")

	node := g.Nodes["dev/auth"]
	if node.Title != "Auth v2" {
		t.Errorf("title should be updated: got %q", node.Title)
	}
	if node.Confidence != 0.9 {
		t.Errorf("confidence should be preserved at 0.9, got %f", node.Confidence)
	}
	if node.Source != "inferred" {
		t.Errorf("source should be updated: got %q", node.Source)
	}
}

func TestMergeConfidenceUpgrade(t *testing.T) {
	g := NewGraph()

	// First merge with low confidence.
	g.Merge(&Manifest{
		Nodes: []ManifestNode{
			{ID: "dev/auth", Agent: "developer", Title: "Auth v1", Summary: "S", Confidence: 0.3},
		},
	}, "req-1", "first")

	// Second merge with higher confidence — should update.
	g.Merge(&Manifest{
		Nodes: []ManifestNode{
			{ID: "dev/auth", Agent: "developer", Title: "Auth v2", Summary: "S2", Confidence: 0.95},
		},
	}, "req-2", "second")

	if g.Nodes["dev/auth"].Confidence != 0.95 {
		t.Errorf("confidence should be upgraded to 0.95, got %f", g.Nodes["dev/auth"].Confidence)
	}
}

func TestMergeTTLNotOverwrittenByZero(t *testing.T) {
	g := NewGraph()

	g.Merge(&Manifest{
		Nodes: []ManifestNode{
			{ID: "dev/auth", Agent: "developer", Title: "Auth", Summary: "S", TTLDays: 60},
		},
	}, "req-1", "first")

	// Second merge with TTLDays=0 — should NOT overwrite existing TTL.
	g.Merge(&Manifest{
		Nodes: []ManifestNode{
			{ID: "dev/auth", Agent: "developer", Title: "Auth", Summary: "S", TTLDays: 0},
		},
	}, "req-2", "second")

	if g.Nodes["dev/auth"].TTLDays != 60 {
		t.Errorf("ttl_days should be preserved at 60, got %d", g.Nodes["dev/auth"].TTLDays)
	}
}

// tieredMemStorageGraph is a tier-aware in-memory storage for testing LoadFromTier/SaveToTier.
type tieredMemStorageGraph struct {
	files map[runtime.StorageTier]map[string][]byte
}

func newTieredMemStorageGraph() *tieredMemStorageGraph {
	return &tieredMemStorageGraph{
		files: map[runtime.StorageTier]map[string][]byte{
			runtime.TierShared:  {},
			runtime.TierAgent:   {},
			runtime.TierScratch: {},
		},
	}
}

func (m *tieredMemStorageGraph) Read(_ context.Context, tier runtime.StorageTier, path string) ([]byte, error) {
	data, ok := m.files[tier][path]
	if !ok {
		return nil, &notFoundError{path}
	}
	return data, nil
}

func (m *tieredMemStorageGraph) Write(_ context.Context, tier runtime.StorageTier, path string, data []byte) error {
	m.files[tier][path] = data
	return nil
}

func (m *tieredMemStorageGraph) Exists(_ context.Context, tier runtime.StorageTier, path string) (bool, error) {
	_, ok := m.files[tier][path]
	return ok, nil
}

func TestLoadFromTierSaveToTierRoundTrip(t *testing.T) {
	store := newTieredMemStorageGraph()
	ctx := context.Background()

	g := NewGraph()
	g.Merge(&Manifest{
		Nodes: []ManifestNode{
			{ID: "dev/auth", Agent: "developer", Title: "Auth", Summary: "Auth module"},
		},
		Edges: []Edge{
			{From: "dev/auth", To: "arch/api", Relation: "implements"},
		},
	}, "req-1", "test")

	// Save to TierAgent.
	if err := SaveToTier(ctx, store, runtime.TierAgent, g); err != nil {
		t.Fatalf("save to agent tier: %v", err)
	}

	// Load from TierAgent.
	loaded, err := LoadFromTier(ctx, store, runtime.TierAgent)
	if err != nil {
		t.Fatalf("load from agent tier: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected non-nil graph")
	}
	if loaded.Version != g.Version {
		t.Errorf("version: got %d, want %d", loaded.Version, g.Version)
	}
	if len(loaded.Nodes) != 1 {
		t.Fatalf("nodes: got %d, want 1", len(loaded.Nodes))
	}
	if loaded.Nodes["dev/auth"].Title != "Auth" {
		t.Errorf("title: got %q", loaded.Nodes["dev/auth"].Title)
	}
	if len(loaded.Edges) != 1 {
		t.Errorf("edges: got %d, want 1", len(loaded.Edges))
	}

	// Loading from a different tier should return nil (no data there).
	other, err := LoadFromTier(ctx, store, runtime.TierShared)
	if err != nil {
		t.Fatalf("load from shared tier: %v", err)
	}
	if other != nil {
		t.Error("expected nil graph from different tier")
	}
}

func TestLoadFromTierNonExistent(t *testing.T) {
	store := newMemStorage()
	g, err := LoadFromTier(context.Background(), store, runtime.TierAgent)
	if err != nil {
		t.Fatalf("load non-existent: %v", err)
	}
	if g != nil {
		t.Error("expected nil graph for non-existent file")
	}
}

func TestIsSuperseded(t *testing.T) {
	g := NewGraph()
	g.Nodes["arch/bootstrap"] = &Node{
		ID: "arch/bootstrap", Agent: "architect", Title: "Use Bootstrap",
		Tags: []string{"decision", "design-system"}, TTLDays: 0,
	}
	g.Nodes["arch/md3"] = &Node{
		ID: "arch/md3", Agent: "architect", Title: "Use Material Design 3",
		Tags: []string{"decision", "design-system"}, TTLDays: 0,
	}
	g.Edges = []*Edge{
		{From: "arch/md3", To: "arch/bootstrap", Relation: "supersedes"},
	}

	if !g.IsSuperseded("arch/bootstrap") {
		t.Error("arch/bootstrap should be superseded by arch/md3")
	}
	if g.IsSuperseded("arch/md3") {
		t.Error("arch/md3 should NOT be superseded")
	}
}

func TestIsSupersededStaleSuperseder(t *testing.T) {
	g := NewGraph()
	g.Nodes["arch/old"] = &Node{
		ID: "arch/old", Agent: "architect", Title: "Old Decision",
		Tags: []string{"decision"}, TTLDays: 0,
	}
	g.Nodes["arch/newer"] = &Node{
		ID: "arch/newer", Agent: "architect", Title: "Newer Decision",
		Tags:      []string{"decision"},
		TTLDays:   1,
		UpdatedAt: time.Now().Add(-48 * time.Hour), // stale
	}
	g.Edges = []*Edge{
		{From: "arch/newer", To: "arch/old", Relation: "supersedes"},
	}

	// Since the superseding node is stale, the old node is NOT superseded.
	if g.IsSuperseded("arch/old") {
		t.Error("arch/old should NOT be superseded when superseder is stale")
	}
}

func TestIsSupersededNoEdge(t *testing.T) {
	g := NewGraph()
	g.Nodes["arch/solo"] = &Node{
		ID: "arch/solo", Agent: "architect", Title: "Solo Decision",
		Tags: []string{"decision"}, TTLDays: 0,
	}

	if g.IsSuperseded("arch/solo") {
		t.Error("node with no supersedes edge should not be superseded")
	}
	if g.IsSuperseded("nonexistent") {
		t.Error("nonexistent node should not be superseded")
	}
}

func TestDetectDecisionConflicts(t *testing.T) {
	g := NewGraph()
	g.Nodes["arch/md3"] = &Node{
		ID: "arch/md3", Agent: "architect", Title: "Use MD3",
		Tags: []string{"decision", "design-system"}, TTLDays: 0,
	}
	g.Nodes["designer/fluent"] = &Node{
		ID: "designer/fluent", Agent: "designer", Title: "Use Fluent UI",
		Tags: []string{"decision", "design-system"}, TTLDays: 0,
	}
	g.Nodes["arch/rest-api"] = &Node{
		ID: "arch/rest-api", Agent: "architect", Title: "Use REST",
		Tags: []string{"decision", "api"}, TTLDays: 0,
	}

	conflicts := g.DetectDecisionConflicts()
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflicts))
	}
	c := conflicts[0]
	if c.Tag != "design-system" {
		t.Errorf("expected conflict on 'design-system', got %q", c.Tag)
	}
}

func TestDetectDecisionConflictsNone(t *testing.T) {
	g := NewGraph()
	g.Nodes["arch/md3"] = &Node{
		ID: "arch/md3", Agent: "architect", Title: "Use MD3",
		Tags: []string{"decision", "design-system"}, TTLDays: 0,
	}
	g.Nodes["arch/rest"] = &Node{
		ID: "arch/rest", Agent: "architect", Title: "Use REST",
		Tags: []string{"decision", "api"}, TTLDays: 0,
	}

	conflicts := g.DetectDecisionConflicts()
	if len(conflicts) != 0 {
		t.Fatalf("expected 0 conflicts (different domains), got %d", len(conflicts))
	}
}

func TestDetectDecisionConflictsSkipsSuperseded(t *testing.T) {
	g := NewGraph()
	g.Nodes["arch/bootstrap"] = &Node{
		ID: "arch/bootstrap", Agent: "architect", Title: "Use Bootstrap",
		Tags: []string{"decision", "design-system"}, TTLDays: 0,
	}
	g.Nodes["arch/md3"] = &Node{
		ID: "arch/md3", Agent: "architect", Title: "Use MD3",
		Tags: []string{"decision", "design-system"}, TTLDays: 0,
	}
	g.Edges = []*Edge{
		{From: "arch/md3", To: "arch/bootstrap", Relation: "supersedes"},
	}

	conflicts := g.DetectDecisionConflicts()
	if len(conflicts) != 0 {
		t.Fatalf("expected 0 conflicts (bootstrap is superseded), got %d", len(conflicts))
	}
}

func TestActiveDecisionsOrderedByRecency(t *testing.T) {
	g := NewGraph()
	now := time.Now()
	g.Nodes["arch/old"] = &Node{
		ID: "arch/old", Agent: "architect", Title: "Old",
		Tags: []string{"decision"}, TTLDays: 0, UpdatedAt: now.Add(-24 * time.Hour),
	}
	g.Nodes["arch/new"] = &Node{
		ID: "arch/new", Agent: "architect", Title: "New",
		Tags: []string{"decision"}, TTLDays: 0, UpdatedAt: now,
	}

	decisions := g.ActiveDecisions()
	if len(decisions) != 2 {
		t.Fatalf("expected 2 decisions, got %d", len(decisions))
	}
	if decisions[0].ID != "arch/new" {
		t.Errorf("expected newest first, got %s", decisions[0].ID)
	}
	if decisions[1].ID != "arch/old" {
		t.Errorf("expected oldest second, got %s", decisions[1].ID)
	}
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
