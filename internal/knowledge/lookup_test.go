package knowledge

import (
	"testing"
	"time"
)

// helper to build a graph with given nodes and edges.
func testGraph(nodes []*Node, edges []*Edge) *Graph {
	g := NewGraph()
	for _, n := range nodes {
		g.Nodes[n.ID] = n
	}
	g.Edges = edges
	return g
}

func TestLookupOwnNodesOnly(t *testing.T) {
	g := testGraph([]*Node{
		{ID: "dev/auth", Agent: "developer", Title: "Auth module", Summary: "Handles authentication"},
		{ID: "dev/db", Agent: "developer", Title: "DB layer", Summary: "Database access"},
		{ID: "arch/api", Agent: "architect", Title: "API design", Summary: "REST API design"},
	}, nil)

	result := Lookup(g, "developer", "implement auth", LookupOpts{})
	if len(result.Own) != 2 {
		t.Fatalf("expected 2 own nodes, got %d", len(result.Own))
	}
	if len(result.Related) != 0 {
		t.Fatalf("expected 0 related nodes, got %d", len(result.Related))
	}
}

func TestLookupOneHop(t *testing.T) {
	g := testGraph([]*Node{
		{ID: "dev/auth", Agent: "developer", Title: "Auth impl", Summary: "Auth implementation"},
		{ID: "arch/api", Agent: "architect", Title: "API design", Summary: "REST API design"},
	}, []*Edge{
		{From: "arch/api", To: "dev/auth", Relation: "implements"},
	})

	result := Lookup(g, "developer", "implement auth", LookupOpts{})
	if len(result.Own) != 1 {
		t.Fatalf("expected 1 own node, got %d", len(result.Own))
	}
	if len(result.Related) != 1 {
		t.Fatalf("expected 1 related node, got %d", len(result.Related))
	}
	rn := result.Related[0]
	if rn.ID != "arch/api" {
		t.Errorf("expected related node arch/api, got %s", rn.ID)
	}
	if rn.Depth != 1 {
		t.Errorf("expected depth 1, got %d", rn.Depth)
	}
}

func TestLookupTwoHops(t *testing.T) {
	g := testGraph([]*Node{
		{ID: "dev/auth", Agent: "developer", Title: "Auth impl", Summary: "Auth implementation"},
		{ID: "arch/api", Agent: "architect", Title: "API design", Summary: "REST API design"},
		{ID: "arch/system", Agent: "architect", Title: "System arch", Summary: "Overall system architecture"},
	}, []*Edge{
		{From: "arch/api", To: "dev/auth", Relation: "implements"},
		{From: "arch/system", To: "arch/api", Relation: "refines"},
	})

	result := Lookup(g, "developer", "build system", LookupOpts{MaxDepth: 2})
	if len(result.Related) != 2 {
		t.Fatalf("expected 2 related nodes, got %d", len(result.Related))
	}

	// arch/system matches keyword "system" so it's found as a keyword seed (depth 0)
	// rather than via 2-hop BFS. Either way it must be present.
	var found bool
	for _, rn := range result.Related {
		if rn.ID == "arch/system" {
			found = true
		}
	}
	if !found {
		t.Error("arch/system not found in related nodes")
	}
}

func TestLookupRespectMaxDepth(t *testing.T) {
	g := testGraph([]*Node{
		{ID: "dev/auth", Agent: "developer", Title: "Auth impl", Summary: "Auth"},
		{ID: "arch/api", Agent: "architect", Title: "API design", Summary: "API"},
		{ID: "arch/system", Agent: "architect", Title: "System arch", Summary: "System"},
	}, []*Edge{
		{From: "arch/api", To: "dev/auth", Relation: "implements"},
		{From: "arch/system", To: "arch/api", Relation: "refines"},
	})

	result := Lookup(g, "developer", "auth", LookupOpts{MaxDepth: 1})
	if len(result.Related) != 1 {
		t.Fatalf("expected 1 related node at depth 1, got %d", len(result.Related))
	}
	if result.Related[0].ID != "arch/api" {
		t.Errorf("expected arch/api, got %s", result.Related[0].ID)
	}
}

func TestLookupRespectMaxNodes(t *testing.T) {
	nodes := []*Node{
		{ID: "dev/core", Agent: "developer", Title: "Core", Summary: "Core module"},
	}
	var edges []*Edge
	// Create 5 architect nodes all connected to dev/core.
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		id := "arch/" + name
		nodes = append(nodes, &Node{ID: id, Agent: "architect", Title: "Design " + name, Summary: "Design " + name})
		edges = append(edges, &Edge{From: id, To: "dev/core", Relation: "implements"})
	}
	g := testGraph(nodes, edges)

	result := Lookup(g, "developer", "core", LookupOpts{MaxNodes: 3})
	if len(result.Related) != 3 {
		t.Fatalf("expected 3 related nodes (MaxNodes cap), got %d", len(result.Related))
	}
}

func TestLookupBidirectional(t *testing.T) {
	g := testGraph([]*Node{
		{ID: "dev/auth", Agent: "developer", Title: "Auth impl", Summary: "Auth"},
		{ID: "arch/api", Agent: "architect", Title: "API design", Summary: "API"},
	}, []*Edge{
		// Edge points FROM dev TO arch — BFS should still find arch/api from developer seeds.
		{From: "dev/auth", To: "arch/api", Relation: "depends_on"},
	})

	result := Lookup(g, "developer", "auth", LookupOpts{})
	if len(result.Related) != 1 {
		t.Fatalf("expected 1 related node via bidirectional traversal, got %d", len(result.Related))
	}
	if result.Related[0].ID != "arch/api" {
		t.Errorf("expected arch/api, got %s", result.Related[0].ID)
	}
}

func TestLookupKeywordBoost(t *testing.T) {
	g := testGraph([]*Node{
		{ID: "dev/core", Agent: "developer", Title: "Core", Summary: "Core module"},
		{ID: "arch/auth", Agent: "architect", Title: "Auth design", Summary: "Authentication design with tokens"},
		{ID: "arch/db", Agent: "architect", Title: "DB design", Summary: "Database schema design"},
	}, []*Edge{
		{From: "arch/auth", To: "dev/core", Relation: "implements"},
		{From: "arch/db", To: "dev/core", Relation: "implements"},
	})

	// Task description mentions "authentication" and "tokens" — should boost arch/auth.
	result := Lookup(g, "developer", "implement authentication with tokens", LookupOpts{})
	if len(result.Related) < 2 {
		t.Fatalf("expected at least 2 related nodes, got %d", len(result.Related))
	}
	// arch/auth should rank first due to keyword overlap.
	if result.Related[0].ID != "arch/auth" {
		t.Errorf("expected arch/auth to rank first, got %s", result.Related[0].ID)
	}
}

func TestLookupDanglingEdge(t *testing.T) {
	g := testGraph([]*Node{
		{ID: "dev/auth", Agent: "developer", Title: "Auth", Summary: "Auth"},
	}, []*Edge{
		{From: "dev/auth", To: "nonexistent/node", Relation: "depends_on"},
	})

	result := Lookup(g, "developer", "auth", LookupOpts{})
	// Should not panic, dangling edge is skipped.
	if len(result.Related) != 0 {
		t.Fatalf("expected 0 related nodes (dangling edge skipped), got %d", len(result.Related))
	}
}

func TestLookupEmptyGraph(t *testing.T) {
	// Nil graph.
	result := Lookup(nil, "developer", "task", LookupOpts{})
	if len(result.Own) != 0 || len(result.Related) != 0 {
		t.Fatal("expected empty result for nil graph")
	}

	// Empty graph.
	g := NewGraph()
	result = Lookup(g, "developer", "task", LookupOpts{})
	if len(result.Own) != 0 || len(result.Related) != 0 {
		t.Fatal("expected empty result for empty graph")
	}
}

func TestTokenize(t *testing.T) {
	tokens := tokenize("Implement the Auth-Module (v2) quickly!")
	expected := map[string]bool{
		"implement": true,
		"the":       true,
		"auth":      true,
		"module":    true,
		"quickly":   true,
	}
	// "v2" is only 2 chars — should be filtered out.
	if tokens["v2"] {
		t.Error("expected v2 to be filtered (< 3 chars)")
	}
	for tok := range expected {
		if !tokens[tok] {
			t.Errorf("expected token %q", tok)
		}
	}
}

func TestComputeScore(t *testing.T) {
	node := &Node{
		Title:   "API Design",
		Summary: "REST API authentication design",
		Tags:    []string{"api", "auth"},
	}

	// No keywords, no confidence — pure depth score.
	score1 := computeScore(node, 1, nil)
	if score1 != 1.0 {
		t.Errorf("expected 1.0 for depth=1 no keywords no confidence, got %f", score1)
	}

	score2 := computeScore(node, 2, nil)
	if score2 != 0.5 {
		t.Errorf("expected 0.5 for depth=2 no keywords no confidence, got %f", score2)
	}

	// With keywords.
	keywords := tokenize("api authentication design")
	score := computeScore(node, 1, keywords)
	// depth=1 -> 1.0, confidence=0 -> 0, keywords: all 3 match -> 1.0
	// total = 1.0 + 1.0 + 0 = 2.0
	if score != 2.0 {
		t.Errorf("expected 2.0 for depth=1 with 3/3 keyword overlap, got %f", score)
	}
}

func TestComputeScoreWithConfidence(t *testing.T) {
	node := &Node{
		Title:      "API Design",
		Summary:    "REST API",
		Confidence: 0.8,
	}

	// No keywords: depth=1 -> 1.0, confidence=0.8 -> 0.4, total=1.4
	score := computeScore(node, 1, nil)
	expected := 1.0 + 0.8*0.5
	if score != expected {
		t.Errorf("expected %f, got %f", expected, score)
	}
}

func TestLookupSkipsStaleOwnNodes(t *testing.T) {
	g := testGraph([]*Node{
		{ID: "dev/fresh", Agent: "developer", Title: "Fresh", Summary: "Fresh node",
			UpdatedAt: time.Now(), TTLDays: 30},
		{ID: "dev/stale", Agent: "developer", Title: "Stale", Summary: "Stale node",
			UpdatedAt: time.Now().Add(-60 * 24 * time.Hour), TTLDays: 30},
	}, nil)

	result := Lookup(g, "developer", "task", LookupOpts{})
	if len(result.Own) != 1 {
		t.Fatalf("expected 1 own node (stale excluded), got %d", len(result.Own))
	}
	if result.Own[0].ID != "dev/fresh" {
		t.Errorf("expected dev/fresh, got %s", result.Own[0].ID)
	}
}

func TestLookupSkipsStaleRelatedNodes(t *testing.T) {
	g := testGraph([]*Node{
		{ID: "dev/auth", Agent: "developer", Title: "Auth", Summary: "Auth",
			UpdatedAt: time.Now()},
		{ID: "arch/fresh", Agent: "architect", Title: "Fresh Design", Summary: "Fresh",
			UpdatedAt: time.Now(), TTLDays: 30},
		{ID: "arch/stale", Agent: "architect", Title: "Stale Design", Summary: "Stale",
			UpdatedAt: time.Now().Add(-60 * 24 * time.Hour), TTLDays: 30},
	}, []*Edge{
		{From: "arch/fresh", To: "dev/auth", Relation: "implements"},
		{From: "arch/stale", To: "dev/auth", Relation: "implements"},
	})

	result := Lookup(g, "developer", "auth", LookupOpts{})
	if len(result.Related) != 1 {
		t.Fatalf("expected 1 related node (stale excluded), got %d", len(result.Related))
	}
	if result.Related[0].ID != "arch/fresh" {
		t.Errorf("expected arch/fresh, got %s", result.Related[0].ID)
	}
}

func TestLookupDualOwnFromAgentGraph(t *testing.T) {
	// Agent graph has full detail with file paths.
	agentGraph := testGraph([]*Node{
		{ID: "dev/auth", Agent: "developer", Title: "Auth", Summary: "Auth module", FilePath: "docs/auth.md"},
		{ID: "dev/db", Agent: "developer", Title: "DB", Summary: "DB layer", FilePath: "docs/db.md"},
	}, nil)

	// Shared graph has reference nodes (no file paths) + edges.
	sharedGraph := testGraph([]*Node{
		{ID: "dev/auth", Agent: "developer", Title: "Auth", Summary: "Auth module"},
		{ID: "dev/db", Agent: "developer", Title: "DB", Summary: "DB layer"},
		{ID: "arch/api", Agent: "architect", Title: "API", Summary: "API design"},
	}, []*Edge{
		{From: "arch/api", To: "dev/auth", Relation: "implements"},
	})

	result := LookupDual(agentGraph, sharedGraph, "developer", "auth", LookupOpts{})

	// Own nodes should come from agentGraph (with file paths).
	if len(result.Own) != 2 {
		t.Fatalf("expected 2 own nodes, got %d", len(result.Own))
	}
	foundFilePath := false
	for _, n := range result.Own {
		if n.ID == "dev/auth" && n.FilePath == "docs/auth.md" {
			foundFilePath = true
		}
	}
	if !foundFilePath {
		t.Error("own node dev/auth should have file path from agent graph")
	}

	// Related nodes should come from sharedGraph BFS.
	if len(result.Related) != 1 {
		t.Fatalf("expected 1 related node, got %d", len(result.Related))
	}
	if result.Related[0].ID != "arch/api" {
		t.Errorf("expected arch/api related, got %s", result.Related[0].ID)
	}
}

func TestLookupDualNilAgentGraph(t *testing.T) {
	sharedGraph := testGraph([]*Node{
		{ID: "dev/auth", Agent: "developer", Title: "Auth", Summary: "Auth module"},
		{ID: "arch/api", Agent: "architect", Title: "API", Summary: "API design"},
	}, []*Edge{
		{From: "arch/api", To: "dev/auth", Relation: "implements"},
	})

	// Should fall back to single-graph Lookup.
	result := LookupDual(nil, sharedGraph, "developer", "auth", LookupOpts{})
	if len(result.Own) != 1 {
		t.Fatalf("expected 1 own node (fallback), got %d", len(result.Own))
	}
	if len(result.Related) != 1 {
		t.Fatalf("expected 1 related node (fallback), got %d", len(result.Related))
	}
}

func TestLookupDualNilSharedGraph(t *testing.T) {
	agentGraph := testGraph([]*Node{
		{ID: "dev/auth", Agent: "developer", Title: "Auth", Summary: "Auth module", FilePath: "docs/auth.md"},
	}, nil)

	result := LookupDual(agentGraph, nil, "developer", "auth", LookupOpts{})
	if len(result.Own) != 1 {
		t.Fatalf("expected 1 own node, got %d", len(result.Own))
	}
	if len(result.Related) != 0 {
		t.Fatalf("expected 0 related nodes (no shared graph), got %d", len(result.Related))
	}
}

func TestLookupSkipsSupersededOwnNodes(t *testing.T) {
	g := testGraph([]*Node{
		{ID: "dev/old-impl", Agent: "developer", Title: "Old Impl", Summary: "Old implementation"},
		{ID: "dev/new-impl", Agent: "developer", Title: "New Impl", Summary: "New implementation"},
	}, []*Edge{
		{From: "dev/new-impl", To: "dev/old-impl", Relation: "supersedes"},
	})

	result := Lookup(g, "developer", "impl", LookupOpts{})
	if len(result.Own) != 1 {
		t.Fatalf("expected 1 own node (superseded excluded), got %d", len(result.Own))
	}
	if result.Own[0].ID != "dev/new-impl" {
		t.Errorf("expected dev/new-impl, got %s", result.Own[0].ID)
	}
}

func TestLookupSkipsSupersededRelatedNodes(t *testing.T) {
	g := testGraph([]*Node{
		{ID: "dev/core", Agent: "developer", Title: "Core", Summary: "Core module"},
		{ID: "arch/bootstrap", Agent: "architect", Title: "Use Bootstrap", Summary: "Bootstrap design",
			Tags: []string{"decision"}},
		{ID: "arch/md3", Agent: "architect", Title: "Use Material Design 3", Summary: "MD3 design",
			Tags: []string{"decision"}},
	}, []*Edge{
		{From: "arch/bootstrap", To: "dev/core", Relation: "implements"},
		{From: "arch/md3", To: "dev/core", Relation: "implements"},
		{From: "arch/md3", To: "arch/bootstrap", Relation: "supersedes"},
	})

	result := Lookup(g, "developer", "core", LookupOpts{})
	for _, rn := range result.Related {
		if rn.ID == "arch/bootstrap" {
			t.Error("superseded node arch/bootstrap should not appear in related nodes")
		}
	}
	found := false
	for _, rn := range result.Related {
		if rn.ID == "arch/md3" {
			found = true
		}
	}
	if !found {
		t.Error("non-superseded node arch/md3 should appear in related nodes")
	}
}

func TestLookupDualSkipsSuperseded(t *testing.T) {
	agentGraph := testGraph([]*Node{
		{ID: "dev/core", Agent: "developer", Title: "Core", Summary: "Core module", FilePath: "docs/core.md"},
	}, nil)

	sharedGraph := testGraph([]*Node{
		{ID: "dev/core", Agent: "developer", Title: "Core", Summary: "Core module"},
		{ID: "arch/old", Agent: "architect", Title: "Old Decision", Summary: "Superseded",
			Tags: []string{"decision"}},
		{ID: "arch/new", Agent: "architect", Title: "New Decision", Summary: "Current",
			Tags: []string{"decision"}},
	}, []*Edge{
		{From: "arch/old", To: "dev/core", Relation: "implements"},
		{From: "arch/new", To: "dev/core", Relation: "implements"},
		{From: "arch/new", To: "arch/old", Relation: "supersedes"},
	})

	result := LookupDual(agentGraph, sharedGraph, "developer", "core", LookupOpts{})
	for _, rn := range result.Related {
		if rn.ID == "arch/old" {
			t.Error("superseded node arch/old should not appear in LookupDual results")
		}
	}
	found := false
	for _, rn := range result.Related {
		if rn.ID == "arch/new" {
			found = true
		}
	}
	if !found {
		t.Error("non-superseded node arch/new should appear in LookupDual results")
	}
}

func TestLookupConfidenceScoring(t *testing.T) {
	g := testGraph([]*Node{
		{ID: "dev/core", Agent: "developer", Title: "Core", Summary: "Core module",
			UpdatedAt: time.Now()},
		{ID: "arch/high", Agent: "architect", Title: "High confidence", Summary: "Design",
			Confidence: 0.9, UpdatedAt: time.Now()},
		{ID: "arch/low", Agent: "architect", Title: "Low confidence", Summary: "Design",
			Confidence: 0.1, UpdatedAt: time.Now()},
	}, []*Edge{
		{From: "arch/high", To: "dev/core", Relation: "implements"},
		{From: "arch/low", To: "dev/core", Relation: "implements"},
	})

	result := Lookup(g, "developer", "core", LookupOpts{})
	if len(result.Related) < 2 {
		t.Fatalf("expected 2 related nodes, got %d", len(result.Related))
	}
	// Higher confidence node should rank first.
	if result.Related[0].ID != "arch/high" {
		t.Errorf("expected arch/high to rank first (higher confidence), got %s", result.Related[0].ID)
	}
}
