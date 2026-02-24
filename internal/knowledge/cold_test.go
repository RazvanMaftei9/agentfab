package knowledge

import (
	"testing"
	"time"
)

func TestNodeIsCold(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name     string
		node     Node
		minHits  int64
		coldDays int
		want     bool
	}{
		{
			name:     "high hits not cold",
			node:     Node{Hits: 5, LastHitAt: now.Add(-60 * 24 * time.Hour)},
			minHits:  3,
			coldDays: 30,
			want:     false,
		},
		{
			name:     "low hits recently accessed not cold",
			node:     Node{Hits: 1, LastHitAt: now.Add(-10 * 24 * time.Hour)},
			minHits:  3,
			coldDays: 30,
			want:     false,
		},
		{
			name:     "low hits old access is cold",
			node:     Node{Hits: 1, LastHitAt: now.Add(-45 * 24 * time.Hour)},
			minHits:  3,
			coldDays: 30,
			want:     true,
		},
		{
			name:     "never accessed old creation is cold",
			node:     Node{Hits: 0, CreatedAt: now.Add(-45 * 24 * time.Hour)},
			minHits:  3,
			coldDays: 30,
			want:     true,
		},
		{
			name:     "never accessed recent creation not cold",
			node:     Node{Hits: 0, CreatedAt: now.Add(-10 * 24 * time.Hour)},
			minHits:  3,
			coldDays: 30,
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.node.IsCold(tt.minHits, tt.coldDays)
			if got != tt.want {
				t.Errorf("IsCold() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMoveToCold_BasicEviction(t *testing.T) {
	now := time.Now()
	g := &Graph{
		Version: 1,
		Nodes: map[string]*Node{
			"a/cold-node": {
				ID:        "a/cold-node",
				Agent:     "a",
				Title:     "Cold",
				Hits:      0,
				CreatedAt: now.Add(-60 * 24 * time.Hour),
			},
			"a/hot-node": {
				ID:        "a/hot-node",
				Agent:     "a",
				Title:     "Hot",
				Hits:      10,
				LastHitAt: now.Add(-1 * time.Hour),
			},
		},
		Edges: []*Edge{
			{From: "a/cold-node", To: "a/hot-node", Relation: "related_to"},
		},
	}

	store := newMemStorage()
	moved, _, err := MoveToCold(t.Context(), store, g, ColdStorageOpts{
		MinHits:       3,
		ColdDays:      30,
		RetentionDays: 1095,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(moved) != 1 || moved[0] != "a/cold-node" {
		t.Errorf("expected [a/cold-node] moved, got %v", moved)
	}
	if _, ok := g.Nodes["a/cold-node"]; ok {
		t.Error("cold node should be removed from active graph")
	}
	if _, ok := g.Nodes["a/hot-node"]; !ok {
		t.Error("hot node should remain in active graph")
	}
	// Edge should be removed from active since one endpoint is gone.
	if len(g.Edges) != 0 {
		t.Errorf("expected 0 active edges, got %d", len(g.Edges))
	}
}

func TestMoveToCold_PreservesActive(t *testing.T) {
	now := time.Now()
	g := &Graph{
		Version: 1,
		Nodes: map[string]*Node{
			"a/active": {
				ID:        "a/active",
				Agent:     "a",
				Title:     "Active",
				Hits:      5,
				LastHitAt: now.Add(-2 * 24 * time.Hour),
			},
		},
	}

	store := newMemStorage()
	moved, _, err := MoveToCold(t.Context(), store, g, ColdStorageOpts{MinHits: 3, ColdDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) != 0 {
		t.Errorf("no nodes should be moved, got %v", moved)
	}
	if len(g.Nodes) != 1 {
		t.Errorf("active graph should still have 1 node, got %d", len(g.Nodes))
	}
}

func TestMoveToCold_PreservesDecisions(t *testing.T) {
	now := time.Now()
	g := &Graph{
		Version: 1,
		Nodes: map[string]*Node{
			"a/decision-node": {
				ID:        "a/decision-node",
				Agent:     "a",
				Title:     "Important Decision",
				Tags:      []string{"decision", "api"},
				Hits:      0,
				CreatedAt: now.Add(-60 * 24 * time.Hour),
			},
		},
	}

	store := newMemStorage()
	moved, _, err := MoveToCold(t.Context(), store, g, ColdStorageOpts{MinHits: 3, ColdDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) != 0 {
		t.Errorf("decision nodes should not be moved, got %v", moved)
	}
}

func TestPurgeColdExpired(t *testing.T) {
	g := &Graph{
		Version: 1,
		Nodes: map[string]*Node{
			"a/old": {
				ID:        "a/old",
				UpdatedAt: time.Now().Add(-1100 * 24 * time.Hour),
			},
			"a/recent": {
				ID:        "a/recent",
				UpdatedAt: time.Now().Add(-30 * 24 * time.Hour),
			},
		},
		Edges: []*Edge{
			{From: "a/old", To: "a/recent", Relation: "related_to"},
		},
	}

	purged := purgeColdExpired(g, 1095)
	if purged != 1 {
		t.Errorf("expected 1 purged, got %d", purged)
	}
	if _, ok := g.Nodes["a/old"]; ok {
		t.Error("old node should be purged")
	}
	if _, ok := g.Nodes["a/recent"]; !ok {
		t.Error("recent node should remain")
	}
	if len(g.Edges) != 0 {
		t.Errorf("expected 0 edges after purge, got %d", len(g.Edges))
	}
}

func TestLookupCold_KeywordMatch(t *testing.T) {
	g := &Graph{
		Nodes: map[string]*Node{
			"a/api-design": {
				ID:      "a/api-design",
				Title:   "API Design Patterns",
				Summary: "RESTful API design with pagination",
			},
			"a/database": {
				ID:      "a/database",
				Title:   "Database Schema",
				Summary: "PostgreSQL schema for user management",
			},
		},
	}

	results := LookupCold(g, "api design rest", 10)
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	if results[0].ID != "a/api-design" {
		t.Errorf("expected api-design as top result, got %s", results[0].ID)
	}
}

func TestRestoreFromCold(t *testing.T) {
	active := NewGraph()
	active.Nodes["a/existing"] = &Node{ID: "a/existing", Agent: "a"}

	cold := &Graph{
		Nodes: map[string]*Node{
			"a/archived": {ID: "a/archived", Agent: "a", Title: "Archived Node"},
		},
		Edges: []*Edge{
			{From: "a/archived", To: "a/existing", Relation: "related_to"},
		},
	}

	store := newMemStorage()
	err := RestoreFromCold(t.Context(), store, active, cold, []string{"a/archived"})
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := active.Nodes["a/archived"]; !ok {
		t.Error("archived node should be restored to active")
	}
	if _, ok := cold.Nodes["a/archived"]; ok {
		t.Error("archived node should be removed from cold")
	}
	if len(active.Edges) != 1 {
		t.Errorf("expected 1 active edge after restore, got %d", len(active.Edges))
	}
	if len(cold.Edges) != 0 {
		t.Errorf("expected 0 cold edges after restore, got %d", len(cold.Edges))
	}
}

// memStorage is defined in graph_test.go and reused here.
