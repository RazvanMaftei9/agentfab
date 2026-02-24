package knowledge

import (
	"testing"
	"time"
)

func TestPruneStaleNodes(t *testing.T) {
	g := testGraph([]*Node{
		{ID: "dev/fresh", Agent: "developer", Title: "Fresh", UpdatedAt: time.Now(), TTLDays: 30},
		{ID: "dev/stale", Agent: "developer", Title: "Stale", UpdatedAt: time.Now().Add(-60 * 24 * time.Hour), TTLDays: 30},
		{ID: "dev/no-ttl", Agent: "developer", Title: "No TTL", UpdatedAt: time.Now().Add(-999 * 24 * time.Hour)},
	}, []*Edge{
		{From: "dev/fresh", To: "dev/stale", Relation: "related_to"},
		{From: "dev/fresh", To: "dev/no-ttl", Relation: "related_to"},
	})

	evicted := g.Prune(PruneOpts{})
	if len(evicted) != 1 || evicted[0] != "dev/stale" {
		t.Fatalf("expected [dev/stale] evicted, got %v", evicted)
	}
	if _, ok := g.Nodes["dev/stale"]; ok {
		t.Error("stale node should be removed")
	}
	if _, ok := g.Nodes["dev/fresh"]; !ok {
		t.Error("fresh node should remain")
	}
	if _, ok := g.Nodes["dev/no-ttl"]; !ok {
		t.Error("no-ttl node should remain")
	}
	// Edge to stale node should be cleaned up; edge to no-ttl should remain.
	if len(g.Edges) != 1 {
		t.Errorf("expected 1 edge after cleanup, got %d", len(g.Edges))
	}
}

func TestPruneConfidenceFloor(t *testing.T) {
	g := testGraph([]*Node{
		{ID: "dev/high", Agent: "developer", Title: "High", Confidence: 0.9},
		{ID: "dev/low", Agent: "developer", Title: "Low", Confidence: 0.1},
		{ID: "dev/unset", Agent: "developer", Title: "Unset", Confidence: 0},
	}, nil)

	evicted := g.Prune(PruneOpts{ConfidenceFloor: 0.2})
	if len(evicted) != 1 {
		t.Fatalf("expected 1 evicted, got %d: %v", len(evicted), evicted)
	}
	if evicted[0] != "dev/low" {
		t.Errorf("expected dev/low evicted, got %s", evicted[0])
	}
	if _, ok := g.Nodes["dev/unset"]; !ok {
		t.Error("confidence=0 (unset) should NOT be evicted")
	}
}

func TestPruneMaxNodes(t *testing.T) {
	nodes := make([]*Node, 5)
	for i := range nodes {
		nodes[i] = &Node{
			ID:         "dev/" + string(rune('a'+i)),
			Agent:      "developer",
			Title:      string(rune('A' + i)),
			Confidence: float64(i+1) * 0.1, // 0.1, 0.2, 0.3, 0.4, 0.5
		}
	}
	g := testGraph(nodes, nil)

	evicted := g.Prune(PruneOpts{MaxNodes: 3, ConfidenceFloor: 0.01})

	if len(g.Nodes) != 3 {
		t.Fatalf("expected 3 nodes remaining, got %d", len(g.Nodes))
	}
	// Lowest-confidence nodes (0.1, 0.2) should be evicted.
	if _, ok := g.Nodes["dev/a"]; ok {
		t.Error("dev/a (confidence 0.1) should be evicted")
	}
	if _, ok := g.Nodes["dev/b"]; ok {
		t.Error("dev/b (confidence 0.2) should be evicted")
	}
	if len(evicted) != 2 {
		t.Errorf("expected 2 evicted, got %d", len(evicted))
	}
}

func TestPruneDanglingEdges(t *testing.T) {
	g := testGraph([]*Node{
		{ID: "dev/a", Agent: "developer", Title: "A", Confidence: 0.9},
		{ID: "dev/b", Agent: "developer", Title: "B", Confidence: 0.05},
	}, []*Edge{
		{From: "dev/a", To: "dev/b", Relation: "related_to"},
		{From: "dev/a", To: "dev/nonexistent", Relation: "related_to"},
	})

	g.Prune(PruneOpts{ConfidenceFloor: 0.2})

	// dev/b evicted → edge dev/a→dev/b should be cleaned up.
	// dev/nonexistent never existed → edge dev/a→dev/nonexistent also cleaned.
	if len(g.Edges) != 0 {
		t.Errorf("expected 0 edges after pruning, got %d", len(g.Edges))
	}
}

func TestPruneNoEviction(t *testing.T) {
	g := testGraph([]*Node{
		{ID: "dev/a", Agent: "developer", Title: "A", Confidence: 0.9},
		{ID: "dev/b", Agent: "developer", Title: "B", Confidence: 0.8},
	}, []*Edge{
		{From: "dev/a", To: "dev/b", Relation: "related_to"},
	})

	evicted := g.Prune(PruneOpts{MaxNodes: 200, ConfidenceFloor: 0.2})
	if len(evicted) != 0 {
		t.Errorf("expected no evictions, got %v", evicted)
	}
	if len(g.Nodes) != 2 {
		t.Errorf("expected 2 nodes, got %d", len(g.Nodes))
	}
	if len(g.Edges) != 1 {
		t.Errorf("expected 1 edge, got %d", len(g.Edges))
	}
}
