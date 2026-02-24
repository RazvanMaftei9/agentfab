package taskgraph

import (
	"testing"

	"github.com/razvanmaftei/agentfab/internal/loop"
)

func TestValidateValidDAG(t *testing.T) {
	g := &TaskGraph{
		RequestID: "req-1",
		Tasks: []*TaskNode{
			{ID: "t1", Agent: "architect", Description: "design", Status: StatusPending},
			{ID: "t2", Agent: "developer", Description: "implement", DependsOn: []string{"t1"}, Status: StatusPending},
			{ID: "t3", Agent: "architect", Description: "review", DependsOn: []string{"t2"}, Status: StatusPending},
		},
	}
	if err := g.Validate(); err != nil {
		t.Fatalf("expected valid DAG: %v", err)
	}
}

func TestValidateDuplicateID(t *testing.T) {
	g := &TaskGraph{
		Tasks: []*TaskNode{
			{ID: "t1", Agent: "a", Description: "x"},
			{ID: "t1", Agent: "b", Description: "y"},
		},
	}
	if err := g.Validate(); err == nil {
		t.Fatal("expected duplicate ID error")
	}
}

func TestValidateMissingDependency(t *testing.T) {
	g := &TaskGraph{
		Tasks: []*TaskNode{
			{ID: "t1", Agent: "a", Description: "x", DependsOn: []string{"t99"}},
		},
	}
	if err := g.Validate(); err == nil {
		t.Fatal("expected missing dependency error")
	}
}

func TestValidateSelfDependency(t *testing.T) {
	g := &TaskGraph{
		Tasks: []*TaskNode{
			{ID: "t1", Agent: "a", Description: "x", DependsOn: []string{"t1"}},
		},
	}
	if err := g.Validate(); err == nil {
		t.Fatal("expected self-dependency error")
	}
}

func TestValidateCycle(t *testing.T) {
	g := &TaskGraph{
		Tasks: []*TaskNode{
			{ID: "t1", Agent: "a", Description: "x", DependsOn: []string{"t2"}},
			{ID: "t2", Agent: "b", Description: "y", DependsOn: []string{"t1"}},
		},
	}
	if err := g.Validate(); err == nil {
		t.Fatal("expected cycle error")
	}
}

func TestTopologicalSort(t *testing.T) {
	g := &TaskGraph{
		Tasks: []*TaskNode{
			{ID: "t3", Agent: "a", Description: "z", DependsOn: []string{"t1", "t2"}},
			{ID: "t1", Agent: "a", Description: "x"},
			{ID: "t2", Agent: "b", Description: "y", DependsOn: []string{"t1"}},
		},
	}

	sorted, err := g.TopologicalSort()
	if err != nil {
		t.Fatalf("sort: %v", err)
	}

	pos := make(map[string]int)
	for i, id := range sorted {
		pos[id] = i
	}

	if pos["t1"] > pos["t2"] {
		t.Error("t1 should come before t2")
	}
	if pos["t1"] > pos["t3"] {
		t.Error("t1 should come before t3")
	}
	if pos["t2"] > pos["t3"] {
		t.Error("t2 should come before t3")
	}
}

func TestReadyTasks(t *testing.T) {
	g := &TaskGraph{
		Tasks: []*TaskNode{
			{ID: "t1", Agent: "a", Description: "x", Status: StatusCompleted},
			{ID: "t2", Agent: "b", Description: "y", DependsOn: []string{"t1"}, Status: StatusPending},
			{ID: "t3", Agent: "c", Description: "z", DependsOn: []string{"t1", "t2"}, Status: StatusPending},
		},
	}

	ready := g.ReadyTasks()
	if len(ready) != 1 || ready[0].ID != "t2" {
		t.Fatalf("expected [t2], got %v", ready)
	}

	// Complete t2, now t3 should be ready.
	g.Get("t2").Status = StatusCompleted
	ready = g.ReadyTasks()
	if len(ready) != 1 || ready[0].ID != "t3" {
		t.Fatalf("expected [t3], got %v", ready)
	}
}

func TestFailDependentsCascades(t *testing.T) {
	// t1 → t2 → t3 (chain). Failing t1 should cascade to t2 and t3.
	g := &TaskGraph{
		Tasks: []*TaskNode{
			{ID: "t1", Agent: "a", Status: StatusFailed, Result: "error"},
			{ID: "t2", Agent: "b", DependsOn: []string{"t1"}, Status: StatusPending},
			{ID: "t3", Agent: "c", DependsOn: []string{"t2"}, Status: StatusPending},
		},
	}

	cascaded := g.FailDependents("t1")
	if len(cascaded) != 2 {
		t.Fatalf("expected 2 cascaded, got %d: %v", len(cascaded), cascaded)
	}

	for _, id := range []string{"t2", "t3"} {
		task := g.Get(id)
		if task.Status != StatusFailed {
			t.Errorf("%s: expected failed, got %s", id, task.Status)
		}
	}

	if !g.AllDone() {
		t.Error("all tasks should be done after cascade")
	}
}

func TestFailDependentsPartialGraph(t *testing.T) {
	// t1 fails, t2 depends on t1, t3 is independent.
	g := &TaskGraph{
		Tasks: []*TaskNode{
			{ID: "t1", Agent: "a", Status: StatusFailed},
			{ID: "t2", Agent: "b", DependsOn: []string{"t1"}, Status: StatusPending},
			{ID: "t3", Agent: "c", Status: StatusPending},
		},
	}

	cascaded := g.FailDependents("t1")
	if len(cascaded) != 1 || cascaded[0] != "t2" {
		t.Fatalf("expected [t2], got %v", cascaded)
	}

	// t3 should still be pending (independent).
	if g.Get("t3").Status != StatusPending {
		t.Error("t3 should remain pending")
	}

	// t3 should now be ready.
	ready := g.ReadyTasks()
	if len(ready) != 1 || ready[0].ID != "t3" {
		t.Errorf("expected [t3] ready, got %v", ready)
	}
}

func TestFailDependentsNoCascade(t *testing.T) {
	// Leaf task fails, nothing to cascade.
	g := &TaskGraph{
		Tasks: []*TaskNode{
			{ID: "t1", Agent: "a", Status: StatusFailed},
		},
	}

	cascaded := g.FailDependents("t1")
	if len(cascaded) != 0 {
		t.Errorf("expected 0 cascaded, got %d", len(cascaded))
	}
}

func TestFailDependentsSkipsCompleted(t *testing.T) {
	// t2 already completed before t1 fails — should not be re-failed.
	g := &TaskGraph{
		Tasks: []*TaskNode{
			{ID: "t1", Agent: "a", Status: StatusFailed},
			{ID: "t2", Agent: "b", DependsOn: []string{"t1"}, Status: StatusCompleted},
		},
	}

	cascaded := g.FailDependents("t1")
	if len(cascaded) != 0 {
		t.Errorf("expected 0 cascaded, got %d", len(cascaded))
	}
	if g.Get("t2").Status != StatusCompleted {
		t.Error("t2 should remain completed")
	}
}

func testReviewLoop() loop.LoopDefinition {
	return loop.LoopDefinition{
		ID:           "review-1",
		Participants: []string{"developer", "architect"},
		States: []loop.StateConfig{
			{Name: "WORKING", Agent: "developer"},
			{Name: "REVIEWING", Agent: "architect"},
			{Name: "REVISING", Agent: "developer"},
			{Name: "APPROVED"},
			{Name: "ESCALATED"},
		},
		Transitions: []loop.Transition{
			{From: "WORKING", To: "REVIEWING"},
			{From: "REVIEWING", To: "APPROVED", Condition: "approved"},
			{From: "REVIEWING", To: "REVISING", Condition: "revise"},
			{From: "REVISING", To: "REVIEWING"},
		},
		InitialState:   "WORKING",
		TerminalStates: []string{"APPROVED", "ESCALATED"},
		MaxTransitions: 5,
	}
}

func TestValidateWithValidLoop(t *testing.T) {
	g := &TaskGraph{
		RequestID: "req-1",
		Tasks: []*TaskNode{
			{ID: "t1", Agent: "developer", Description: "implement", LoopID: "review-1", Status: StatusPending},
		},
		Loops: []loop.LoopDefinition{testReviewLoop()},
	}
	if err := g.Validate(); err != nil {
		t.Fatalf("expected valid: %v", err)
	}
}

func TestValidateMissingLoopReference(t *testing.T) {
	g := &TaskGraph{
		Tasks: []*TaskNode{
			{ID: "t1", Agent: "developer", Description: "implement", LoopID: "nonexistent", Status: StatusPending},
		},
	}
	if err := g.Validate(); err == nil {
		t.Fatal("expected error for missing loop reference")
	}
}

func TestValidateInvalidLoopDef(t *testing.T) {
	g := &TaskGraph{
		Tasks: []*TaskNode{
			{ID: "t1", Agent: "developer", Description: "implement", Status: StatusPending},
		},
		Loops: []loop.LoopDefinition{{ID: ""}}, // invalid: empty ID
	}
	if err := g.Validate(); err == nil {
		t.Fatal("expected error for invalid loop definition")
	}
}

func TestValidateWithRoster(t *testing.T) {
	g := &TaskGraph{
		Tasks: []*TaskNode{
			{ID: "t1", Agent: "architect", Description: "design", Status: StatusPending},
			{ID: "t2", Agent: "developer", Description: "implement", DependsOn: []string{"t1"}, Status: StatusPending},
		},
	}
	// Valid roster.
	if err := g.ValidateWithRoster([]string{"architect", "developer", "designer"}); err != nil {
		t.Fatalf("expected valid: %v", err)
	}
	// Unknown agent.
	err := g.ValidateWithRoster([]string{"architect"})
	if err == nil {
		t.Fatal("expected error for unknown agent 'developer'")
	}
}

func TestValidateWithRosterLoopAgents(t *testing.T) {
	g := &TaskGraph{
		Tasks: []*TaskNode{
			{ID: "t1", Agent: "developer", Description: "impl", LoopID: "review-1", Status: StatusPending},
		},
		Loops: []loop.LoopDefinition{testReviewLoop()},
	}
	// Architect is in the loop but not in the roster.
	err := g.ValidateWithRoster([]string{"developer"})
	if err == nil {
		t.Fatal("expected error for unknown loop agent 'architect'")
	}
}

func TestValidateLoopFSMCompleteness(t *testing.T) {
	// Non-terminal state REVISING has no outgoing transition.
	g := &TaskGraph{
		Tasks: []*TaskNode{
			{ID: "t1", Agent: "developer", Description: "impl", LoopID: "review-1", Status: StatusPending},
		},
		Loops: []loop.LoopDefinition{{
			ID:           "review-1",
			Participants: []string{"developer", "architect"},
			States: []loop.StateConfig{
				{Name: "WORKING", Agent: "developer"},
				{Name: "REVIEWING", Agent: "architect"},
				{Name: "REVISING", Agent: "developer"},
				{Name: "APPROVED"},
				{Name: "ESCALATED"},
			},
			Transitions: []loop.Transition{
				{From: "WORKING", To: "REVIEWING"},
				{From: "REVIEWING", To: "APPROVED", Condition: "approved"},
				{From: "REVIEWING", To: "REVISING", Condition: "revise"},
				// Missing: {From: "REVISING", To: "REVIEWING"}
			},
			InitialState:   "WORKING",
			TerminalStates: []string{"APPROVED", "ESCALATED"},
			MaxTransitions: 5,
		}},
	}
	err := g.Validate()
	if err == nil {
		t.Fatal("expected error for non-terminal state without outgoing transitions")
	}
}

func TestGetLoop(t *testing.T) {
	g := &TaskGraph{
		Loops: []loop.LoopDefinition{testReviewLoop()},
	}
	if l := g.GetLoop("review-1"); l == nil {
		t.Fatal("expected to find loop")
	}
	if l := g.GetLoop("nonexistent"); l != nil {
		t.Fatal("expected nil for nonexistent loop")
	}
}

func TestAllDone(t *testing.T) {
	g := &TaskGraph{
		Tasks: []*TaskNode{
			{ID: "t1", Status: StatusCompleted},
			{ID: "t2", Status: StatusFailed},
		},
	}
	if !g.AllDone() {
		t.Fatal("expected all done")
	}

	g.Tasks[0].Status = StatusRunning
	if g.AllDone() {
		t.Fatal("should not be done with running task")
	}
}

func TestHasFailuresCancelled(t *testing.T) {
	g := &TaskGraph{
		Tasks: []*TaskNode{
			{ID: "t1", Status: StatusCompleted},
			{ID: "t2", Status: StatusCancelled},
		},
	}
	if !g.HasFailures() {
		t.Fatal("HasFailures should return true with cancelled tasks")
	}
}

func TestHasIncompleteDependentsTrue(t *testing.T) {
	// t1 → t2 → t3: t2 and t3 are pending, so t1 has incomplete dependents.
	g := &TaskGraph{
		Tasks: []*TaskNode{
			{ID: "t1", Agent: "designer", Status: StatusRunning},
			{ID: "t2", Agent: "architect", DependsOn: []string{"t1"}, Status: StatusPending},
			{ID: "t3", Agent: "developer", DependsOn: []string{"t2"}, Status: StatusPending},
		},
	}
	if !g.HasIncompleteDependents("t1") {
		t.Fatal("expected incomplete dependents for t1")
	}
}

func TestHasIncompleteDependentsFalseAllCompleted(t *testing.T) {
	// t1 → t2 → t3: all dependents completed.
	g := &TaskGraph{
		Tasks: []*TaskNode{
			{ID: "t1", Agent: "designer", Status: StatusCompleted},
			{ID: "t2", Agent: "architect", DependsOn: []string{"t1"}, Status: StatusCompleted},
			{ID: "t3", Agent: "developer", DependsOn: []string{"t2"}, Status: StatusCompleted},
		},
	}
	if g.HasIncompleteDependents("t1") {
		t.Fatal("expected no incomplete dependents for t1")
	}
}

func TestHasIncompleteDependentsFalseLeaf(t *testing.T) {
	// t3 is a leaf node with no dependents.
	g := &TaskGraph{
		Tasks: []*TaskNode{
			{ID: "t1", Agent: "designer", Status: StatusCompleted},
			{ID: "t2", Agent: "architect", DependsOn: []string{"t1"}, Status: StatusCompleted},
			{ID: "t3", Agent: "developer", DependsOn: []string{"t2"}, Status: StatusRunning},
		},
	}
	if g.HasIncompleteDependents("t3") {
		t.Fatal("leaf task should have no incomplete dependents")
	}
}

func TestHasIncompleteDependentsTransitive(t *testing.T) {
	// t1 → t2 (completed) → t3 (pending): t1 has incomplete transitive dependent.
	g := &TaskGraph{
		Tasks: []*TaskNode{
			{ID: "t1", Agent: "designer", Status: StatusRunning},
			{ID: "t2", Agent: "architect", DependsOn: []string{"t1"}, Status: StatusCompleted},
			{ID: "t3", Agent: "developer", DependsOn: []string{"t2"}, Status: StatusPending},
		},
	}
	if !g.HasIncompleteDependents("t1") {
		t.Fatal("expected incomplete transitive dependent for t1")
	}
}

func TestAllDoneCancelled(t *testing.T) {
	g := &TaskGraph{
		Tasks: []*TaskNode{
			{ID: "t1", Status: StatusCompleted},
			{ID: "t2", Status: StatusCancelled},
		},
	}
	if !g.AllDone() {
		t.Fatal("AllDone should return true when all tasks are completed/cancelled")
	}
}
