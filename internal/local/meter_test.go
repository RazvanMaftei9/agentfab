package local

import (
	"context"
	"testing"
	"time"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

func TestMeterRecordAndUsage(t *testing.T) {
	m := NewMeter()
	ctx := context.Background()

	m.Record(ctx, runtime.LLMCallRecord{
		AgentName:    "developer",
		InputTokens:  100,
		OutputTokens: 50,
		Duration:     time.Second,
	})
	m.Record(ctx, runtime.LLMCallRecord{
		AgentName:    "developer",
		InputTokens:  200,
		OutputTokens: 100,
		Duration:     2 * time.Second,
	})

	usage, _ := m.Usage(ctx, "developer")
	if usage.InputTokens != 300 {
		t.Errorf("input tokens: got %d, want 300", usage.InputTokens)
	}
	if usage.OutputTokens != 150 {
		t.Errorf("output tokens: got %d, want 150", usage.OutputTokens)
	}
	if usage.TotalCalls != 2 {
		t.Errorf("total calls: got %d, want 2", usage.TotalCalls)
	}
}

func TestMeterBudgetEnforcement(t *testing.T) {
	m := NewMeter()
	ctx := context.Background()

	m.SetBudget(ctx, "developer", runtime.Budget{MaxTotalTokens: 100})

	m.Record(ctx, runtime.LLMCallRecord{
		AgentName:    "developer",
		InputTokens:  60,
		OutputTokens: 50,
	})

	err := m.CheckBudget(ctx, "developer")
	if err == nil {
		t.Fatal("expected budget exceeded error")
	}
}

func TestMeterNoBudget(t *testing.T) {
	m := NewMeter()
	ctx := context.Background()

	m.Record(ctx, runtime.LLMCallRecord{
		AgentName:    "developer",
		InputTokens:  999999,
		OutputTokens: 999999,
	})

	// No budget set — should pass.
	if err := m.CheckBudget(ctx, "developer"); err != nil {
		t.Fatalf("no budget set, should not error: %v", err)
	}
}

func TestMeterModelUsage(t *testing.T) {
	m := NewMeter()
	ctx := context.Background()

	m.Record(ctx, runtime.LLMCallRecord{AgentName: "developer", Model: "anthropic/claude-sonnet-4", InputTokens: 100, OutputTokens: 50})
	m.Record(ctx, runtime.LLMCallRecord{AgentName: "architect", Model: "anthropic/claude-sonnet-4", InputTokens: 200, OutputTokens: 100})
	m.Record(ctx, runtime.LLMCallRecord{AgentName: "developer", Model: "anthropic/claude-haiku-4-5", InputTokens: 50, OutputTokens: 20})

	usages := m.ModelUsage(ctx)

	if len(usages) != 2 {
		t.Fatalf("expected 2 models, got %d", len(usages))
	}

	// Should be sorted alphabetically.
	if usages[0].Model != "anthropic/claude-haiku-4-5" {
		t.Errorf("first model = %q, want anthropic/claude-haiku-4-5", usages[0].Model)
	}
	if usages[0].InputTokens != 50 || usages[0].OutputTokens != 20 {
		t.Errorf("haiku usage: in=%d out=%d, want in=50 out=20", usages[0].InputTokens, usages[0].OutputTokens)
	}
	if usages[0].TotalCalls != 1 {
		t.Errorf("haiku calls = %d, want 1", usages[0].TotalCalls)
	}

	if usages[1].Model != "anthropic/claude-sonnet-4" {
		t.Errorf("second model = %q, want anthropic/claude-sonnet-4", usages[1].Model)
	}
	if usages[1].InputTokens != 300 || usages[1].OutputTokens != 150 {
		t.Errorf("sonnet usage: in=%d out=%d, want in=300 out=150", usages[1].InputTokens, usages[1].OutputTokens)
	}
	if usages[1].TotalCalls != 2 {
		t.Errorf("sonnet calls = %d, want 2", usages[1].TotalCalls)
	}
}

func TestMeterModelUsageEmpty(t *testing.T) {
	m := NewMeter()
	usages := m.ModelUsage(context.Background())
	if len(usages) != 0 {
		t.Errorf("expected 0 models for empty meter, got %d", len(usages))
	}
}

func TestMeterAggregateUsage(t *testing.T) {
	m := NewMeter()
	ctx := context.Background()

	m.Record(ctx, runtime.LLMCallRecord{AgentName: "developer", InputTokens: 100, OutputTokens: 50})
	m.Record(ctx, runtime.LLMCallRecord{AgentName: "architect", InputTokens: 200, OutputTokens: 100})

	usage, _ := m.AggregateUsage(ctx)
	if usage.InputTokens != 300 {
		t.Errorf("aggregate input tokens: got %d, want 300", usage.InputTokens)
	}
	if usage.TotalCalls != 2 {
		t.Errorf("aggregate total calls: got %d, want 2", usage.TotalCalls)
	}
}

func TestMeterRequestUsage(t *testing.T) {
	m := NewMeter()
	ctx := context.Background()

	m.Record(ctx, runtime.LLMCallRecord{AgentName: "developer", RequestID: "req-1", InputTokens: 100, OutputTokens: 50})
	m.Record(ctx, runtime.LLMCallRecord{AgentName: "developer", RequestID: "req-2", InputTokens: 200, OutputTokens: 100})
	m.Record(ctx, runtime.LLMCallRecord{AgentName: "architect", RequestID: "req-1", InputTokens: 300, OutputTokens: 150})

	usage := m.RequestUsage(ctx, "req-1")
	if usage.InputTokens != 400 {
		t.Errorf("req-1 input tokens: got %d, want 400", usage.InputTokens)
	}
	if usage.TotalCalls != 2 {
		t.Errorf("req-1 total calls: got %d, want 2", usage.TotalCalls)
	}

	usage2 := m.RequestUsage(ctx, "req-2")
	if usage2.TotalCalls != 1 {
		t.Errorf("req-2 total calls: got %d, want 1", usage2.TotalCalls)
	}

	usage3 := m.RequestUsage(ctx, "nonexistent")
	if usage3.TotalCalls != 0 {
		t.Errorf("nonexistent total calls: got %d, want 0", usage3.TotalCalls)
	}
}

func TestMeterAllAgentUsage(t *testing.T) {
	m := NewMeter()
	ctx := context.Background()

	m.Record(ctx, runtime.LLMCallRecord{AgentName: "developer", InputTokens: 100, OutputTokens: 50})
	m.Record(ctx, runtime.LLMCallRecord{AgentName: "architect", InputTokens: 200, OutputTokens: 100})
	m.Record(ctx, runtime.LLMCallRecord{AgentName: "developer", InputTokens: 50, OutputTokens: 25})

	all := m.AllAgentUsage(ctx)
	if len(all) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(all))
	}
	if all["developer"].TotalCalls != 2 {
		t.Errorf("developer calls = %d, want 2", all["developer"].TotalCalls)
	}
	if all["architect"].TotalCalls != 1 {
		t.Errorf("architect calls = %d, want 1", all["architect"].TotalCalls)
	}
}

func TestMeterAllRecords(t *testing.T) {
	m := NewMeter()
	ctx := context.Background()

	m.Record(ctx, runtime.LLMCallRecord{AgentName: "developer", InputTokens: 100})
	m.Record(ctx, runtime.LLMCallRecord{AgentName: "architect", InputTokens: 200})

	all := m.AllRecords(ctx)
	if len(all) != 2 {
		t.Errorf("expected 2 records, got %d", len(all))
	}
}
