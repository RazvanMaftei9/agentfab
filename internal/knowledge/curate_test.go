package knowledge

import (
	"testing"
	"time"
)

func TestValidateCurated_PreservesDecisions(t *testing.T) {
	now := time.Now()
	original := &Graph{
		Nodes: map[string]*Node{
			"a/decision": {
				ID:         "a/decision",
				Agent:      "a",
				Title:      "Use React",
				Tags:       []string{"decision", "frontend"},
				Confidence: 0.9,
				CreatedAt:  now,
				UpdatedAt:  now,
			},
			"a/other": {
				ID:        "a/other",
				Agent:     "a",
				Title:     "Other Node",
				CreatedAt: now,
				UpdatedAt: now,
			},
		},
	}

	// Curated graph that removes the decision node — should fail.
	curated := &Graph{
		Nodes: map[string]*Node{
			"a/other": original.Nodes["a/other"],
		},
	}

	err := ValidateCurated(original, curated)
	if err == nil {
		t.Fatal("expected error when decision node is removed")
	}
	if got := err.Error(); got == "" {
		t.Error("error message should not be empty")
	}
}

func TestValidateCurated_PreservesHighConfidence(t *testing.T) {
	now := time.Now()
	original := &Graph{
		Nodes: map[string]*Node{
			"a/high": {
				ID:         "a/high",
				Agent:      "a",
				Title:      "High Confidence",
				Confidence: 0.9,
				CreatedAt:  now,
				UpdatedAt:  now,
			},
			"a/low": {
				ID:         "a/low",
				Agent:      "a",
				Title:      "Low Confidence",
				Confidence: 0.3,
				CreatedAt:  now,
				UpdatedAt:  now,
			},
		},
	}

	// Curated graph removes the high-confidence node — should fail.
	curated := &Graph{
		Nodes: map[string]*Node{
			"a/low": original.Nodes["a/low"],
		},
	}

	err := ValidateCurated(original, curated)
	if err == nil {
		t.Fatal("expected error when high-confidence node is removed")
	}
}

func TestValidateCurated_RejectsNodeIncrease(t *testing.T) {
	now := time.Now()
	original := &Graph{
		Nodes: map[string]*Node{
			"a/one": {ID: "a/one", Agent: "a", CreatedAt: now, UpdatedAt: now},
		},
	}

	curated := &Graph{
		Nodes: map[string]*Node{
			"a/one": original.Nodes["a/one"],
			"a/two": {ID: "a/two", Agent: "a", CreatedAt: now, UpdatedAt: now},
		},
	}

	err := ValidateCurated(original, curated)
	if err == nil {
		t.Fatal("expected error when curated has more nodes than original")
	}
}

func TestValidateCurated_RejectsDanglingEdges(t *testing.T) {
	now := time.Now()
	original := &Graph{
		Nodes: map[string]*Node{
			"a/one": {ID: "a/one", Agent: "a", CreatedAt: now, UpdatedAt: now},
			"a/two": {ID: "a/two", Agent: "a", CreatedAt: now, UpdatedAt: now},
		},
	}

	curated := &Graph{
		Nodes: map[string]*Node{
			"a/one": original.Nodes["a/one"],
		},
		Edges: []*Edge{
			{From: "a/one", To: "a/two", Relation: "related_to"}, // a/two doesn't exist
		},
	}

	err := ValidateCurated(original, curated)
	if err == nil {
		t.Fatal("expected error for dangling edge")
	}
}

func TestValidateCurated_AcceptsValid(t *testing.T) {
	now := time.Now()
	original := &Graph{
		Nodes: map[string]*Node{
			"a/decision": {
				ID:         "a/decision",
				Agent:      "a",
				Title:      "Decision",
				Tags:       []string{"decision"},
				Confidence: 0.9,
				CreatedAt:  now,
				UpdatedAt:  now,
			},
			"a/high": {
				ID:         "a/high",
				Agent:      "a",
				Title:      "High Conf",
				Confidence: 0.85,
				CreatedAt:  now,
				UpdatedAt:  now,
			},
			"a/removable": {
				ID:         "a/removable",
				Agent:      "a",
				Title:      "Removable",
				Confidence: 0.5,
				CreatedAt:  now,
				UpdatedAt:  now,
			},
		},
	}

	curated := &Graph{
		Nodes: map[string]*Node{
			"a/decision": original.Nodes["a/decision"],
			"a/high":     original.Nodes["a/high"],
		},
		Edges: []*Edge{
			{From: "a/decision", To: "a/high", Relation: "related_to"},
		},
	}

	err := ValidateCurated(original, curated)
	if err != nil {
		t.Fatalf("expected valid curated graph, got error: %v", err)
	}
}
