package routing

import (
	"context"
	"fmt"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/razvanmaftei/agentfab/internal/runtime"
)

// mockModel implements model.ChatModel for testing.
type mockModel struct {
	model.ChatModel
	id string
}

func mockFactory(_ context.Context, modelID string) (model.ChatModel, error) {
	return &mockModel{id: modelID}, nil
}

func TestAdaptiveRouter_SelectsFirstTier(t *testing.T) {
	tiers := map[string][]string{
		"developer": {"haiku", "sonnet", "opus"},
	}
	r := NewAdaptiveRouter(mockFactory, tiers)
	factory := r.ModelFactory()

	ctx := runtime.WithAgentName(context.Background(), "developer")
	ctx = runtime.WithTaskID(ctx, "task-1")

	m, err := factory(ctx, "default-model")
	if err != nil {
		t.Fatal(err)
	}
	mm := m.(*mockModel)
	if mm.id != "haiku" {
		t.Errorf("expected haiku, got %s", mm.id)
	}
}

func TestAdaptiveRouter_Escalation(t *testing.T) {
	tiers := map[string][]string{
		"developer": {"haiku", "sonnet", "opus"},
	}
	r := NewAdaptiveRouter(mockFactory, tiers)
	factory := r.ModelFactory()

	ctx := runtime.WithAgentName(context.Background(), "developer")
	ctx = runtime.WithTaskID(ctx, "task-1")

	// First call: tier 0 (haiku)
	m, _ := factory(ctx, "default")
	if m.(*mockModel).id != "haiku" {
		t.Fatal("expected haiku at tier 0")
	}

	// Escalate to tier 1
	r.RecordOutcome("developer", "task-1", false)
	if !r.Escalate("developer", "task-1") {
		t.Fatal("escalation should succeed")
	}

	// Second call: tier 1 (sonnet)
	m, _ = factory(ctx, "default")
	if m.(*mockModel).id != "sonnet" {
		t.Errorf("expected sonnet at tier 1, got %s", m.(*mockModel).id)
	}

	// Escalate to tier 2
	r.RecordOutcome("developer", "task-1", false)
	if !r.Escalate("developer", "task-1") {
		t.Fatal("escalation to tier 2 should succeed")
	}

	// Third call: tier 2 (opus)
	m, _ = factory(ctx, "default")
	if m.(*mockModel).id != "opus" {
		t.Errorf("expected opus at tier 2, got %s", m.(*mockModel).id)
	}

	// Cannot escalate further
	if r.Escalate("developer", "task-1") {
		t.Error("escalation should fail at max tier")
	}
}

func TestAdaptiveRouter_FallsBackForUnknownAgent(t *testing.T) {
	tiers := map[string][]string{
		"developer": {"haiku", "sonnet"},
	}
	r := NewAdaptiveRouter(mockFactory, tiers)
	factory := r.ModelFactory()

	ctx := runtime.WithAgentName(context.Background(), "architect")
	ctx = runtime.WithTaskID(ctx, "task-1")

	m, err := factory(ctx, "default-model")
	if err != nil {
		t.Fatal(err)
	}
	// Unknown agent should use the default model
	if m.(*mockModel).id != "default-model" {
		t.Errorf("expected default-model for unknown agent, got %s", m.(*mockModel).id)
	}
}

func TestAdaptiveRouter_IsolatedPerTask(t *testing.T) {
	tiers := map[string][]string{
		"developer": {"haiku", "sonnet", "opus"},
	}
	r := NewAdaptiveRouter(mockFactory, tiers)
	factory := r.ModelFactory()

	// Escalate task-1 to tier 1
	r.Escalate("developer", "task-1")

	// task-2 should still be at tier 0
	ctx := runtime.WithAgentName(context.Background(), "developer")
	ctx = runtime.WithTaskID(ctx, "task-2")

	m, _ := factory(ctx, "default")
	if m.(*mockModel).id != "haiku" {
		t.Errorf("task-2 should be at tier 0 (haiku), got %s", m.(*mockModel).id)
	}
}

func TestAdaptiveRouter_RecordOutcome(t *testing.T) {
	tiers := map[string][]string{
		"developer": {"haiku", "sonnet"},
	}
	r := NewAdaptiveRouter(mockFactory, tiers)

	r.RecordOutcome("developer", "task-1", true)
	r.RecordOutcome("developer", "task-2", false)
	r.RecordOutcome("developer", "task-3", true)

	stats := r.Report()
	if len(stats) == 0 {
		t.Fatal("expected stats")
	}

	found := false
	for _, s := range stats {
		if s.Agent == "developer" && s.Model == "haiku" {
			found = true
			if s.Success != 2 || s.Failure != 1 {
				t.Errorf("expected 2 success, 1 failure, got %d/%d", s.Success, s.Failure)
			}
		}
	}
	if !found {
		t.Error("expected stats for developer:haiku")
	}
}

func TestTracker_EscalatePastMax(t *testing.T) {
	tr := newTracker()

	// Max tier is 1 (2 tiers: 0, 1)
	_, ok := tr.escalate("dev", "t1", 1)
	if !ok {
		t.Error("first escalation should succeed")
	}
	newTier, ok := tr.escalate("dev", "t1", 1)
	if ok {
		t.Errorf("second escalation should fail, got tier %d", newTier)
	}
}

func TestTracker_Report(t *testing.T) {
	tr := newTracker()

	tr.recordOutcome("dev", "haiku", 0, true)
	tr.recordOutcome("dev", "haiku", 0, true)
	tr.recordOutcome("dev", "haiku", 0, false)
	tr.recordOutcome("dev", "sonnet", 1, true)

	report := tr.report()
	if len(report) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(report))
	}

	for _, s := range report {
		switch s.Model {
		case "haiku":
			if s.Success != 2 || s.Failure != 1 || s.Total != 3 {
				t.Errorf("haiku: expected 2/1/3, got %d/%d/%d", s.Success, s.Failure, s.Total)
			}
		case "sonnet":
			if s.Success != 1 || s.Failure != 0 || s.Total != 1 {
				t.Errorf("sonnet: expected 1/0/1, got %d/%d/%d", s.Success, s.Failure, s.Total)
			}
		default:
			t.Errorf("unexpected model: %s", s.Model)
		}
	}
}

func TestAdaptiveRouter_EscalateUnknownAgent(t *testing.T) {
	tiers := map[string][]string{
		"developer": {"haiku", "sonnet"},
	}
	r := NewAdaptiveRouter(mockFactory, tiers)

	if r.Escalate("architect", "task-1") {
		t.Error("should not escalate unknown agent")
	}
}

func TestAdaptiveRouter_RecordOutcomeUnknownAgent(t *testing.T) {
	tiers := map[string][]string{
		"developer": {"haiku", "sonnet"},
	}
	r := NewAdaptiveRouter(mockFactory, tiers)

	// Should not panic
	r.RecordOutcome("architect", "task-1", true)

	stats := r.Report()
	if len(stats) != 0 {
		t.Errorf("expected no stats for unknown agent, got %d", len(stats))
	}
}

func TestAdaptiveRouter_ConcurrentAccess(t *testing.T) {
	tiers := map[string][]string{
		"developer": {"haiku", "sonnet", "opus"},
	}
	r := NewAdaptiveRouter(mockFactory, tiers)
	factory := r.ModelFactory()

	done := make(chan struct{}, 100)
	for i := 0; i < 100; i++ {
		go func(idx int) {
			defer func() { done <- struct{}{} }()
			ctx := runtime.WithAgentName(context.Background(), "developer")
			ctx = runtime.WithTaskID(ctx, fmt.Sprintf("task-%d", idx))

			factory(ctx, "default")
			r.RecordOutcome("developer", fmt.Sprintf("task-%d", idx), idx%2 == 0)
			r.Escalate("developer", fmt.Sprintf("task-%d", idx))
			factory(ctx, "default")
			r.Report()
		}(i)
	}
	for i := 0; i < 100; i++ {
		<-done
	}
}
