package llm

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/razvanmaftei/agentfab/internal/runtime"
)

// --- helpers ----------------------------------------------------------------

// stubMeter satisfies runtime.Meter but does nothing.
type stubMeter struct{}

func (stubMeter) Record(_ context.Context, _ runtime.LLMCallRecord) error    { return nil }
func (stubMeter) Usage(_ context.Context, _ string) (runtime.UsageSummary, error) {
	return runtime.UsageSummary{}, nil
}
func (stubMeter) AggregateUsage(_ context.Context) (runtime.UsageSummary, error) {
	return runtime.UsageSummary{}, nil
}
func (stubMeter) CheckBudget(_ context.Context, _ string) error  { return nil }
func (stubMeter) SetBudget(_ context.Context, _ string, _ runtime.Budget) error { return nil }

// failNModel streams an error for the first N calls, then succeeds.
type failNModel struct {
	failures int       // how many calls should fail
	calls    atomic.Int64
	failErr  error
}

func (m *failNModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return nil, nil
}

func (m *failNModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	n := m.calls.Add(1)
	if int(n) <= m.failures {
		return nil, m.failErr
	}
	sr, sw := schema.Pipe[*schema.Message](2)
	go func() {
		defer sw.Close()
		sw.Send(&schema.Message{
			Role:    schema.Assistant,
			Content: "ok",
			ResponseMeta: &schema.ResponseMeta{
				Usage: &schema.TokenUsage{PromptTokens: 10, CompletionTokens: 5},
			},
		}, nil)
	}()
	return sr, nil
}

func (m *failNModel) BindTools(_ []*schema.ToolInfo) error { return nil }

// --- tests ------------------------------------------------------------------

func TestIsTransientError(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		transient bool
	}{
		{"nil", nil, false},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"429 status", fmt.Errorf("request failed: status 429"), true},
		{"500 status", fmt.Errorf("status 500 internal server error"), true},
		{"502 Status", fmt.Errorf("Status: 502"), true},
		{"503 status", fmt.Errorf("status 503"), true},
		{"504 status", fmt.Errorf("status 504"), true},
		{"connection reset", fmt.Errorf("read tcp: connection reset by peer"), true},
		{"connection refused", fmt.Errorf("dial tcp: connection refused"), true},
		{"broken pipe", fmt.Errorf("write: broken pipe"), true},
		{"EOF", fmt.Errorf("unexpected EOF"), true},
		{"stream recv", fmt.Errorf("stream recv: timeout"), true},
		{"empty stream", fmt.Errorf("empty stream response"), true},
		{"400 not transient", fmt.Errorf("status 400 bad request"), false},
		{"auth error", fmt.Errorf("authentication failed: invalid API key"), false},
		{"generic error", errors.New("something went wrong"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isTransientError(tt.err)
			if got != tt.transient {
				t.Errorf("isTransientError(%v) = %v, want %v", tt.err, got, tt.transient)
			}
		})
	}
}

func TestGenerateRetry(t *testing.T) {
	mock := &failNModel{
		failures: 2,
		failErr:  fmt.Errorf("status 502 bad gateway"),
	}

	var retryCount atomic.Int64
	m := &MeteredModel{
		Model:     mock,
		AgentName: "test",
		ModelID:   "test-model",
		Meter:     stubMeter{},
		OnRetry: func(attempt, maxAttempts int, err error) {
			retryCount.Add(1)
		},
	}

	resp, err := m.Generate(context.Background(), nil)
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if resp == nil || resp.Content != "ok" {
		t.Fatalf("unexpected response: %v", resp)
	}
	if got := mock.calls.Load(); got != 3 {
		t.Errorf("expected 3 calls (2 failures + 1 success), got %d", got)
	}
	if got := retryCount.Load(); got != 2 {
		t.Errorf("expected 2 OnRetry callbacks, got %d", got)
	}
}

func TestGenerateNoRetryOnNonTransient(t *testing.T) {
	mock := &failNModel{
		failures: 1,
		failErr:  fmt.Errorf("status 400 bad request"),
	}

	m := &MeteredModel{
		Model:     mock,
		AgentName: "test",
		ModelID:   "test-model",
		Meter:     stubMeter{},
	}

	_, err := m.Generate(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for non-transient failure")
	}
	if got := mock.calls.Load(); got != 1 {
		t.Errorf("expected exactly 1 call (no retry for 400), got %d", got)
	}
}
