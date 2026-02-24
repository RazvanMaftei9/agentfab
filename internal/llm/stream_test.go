package llm

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// mockStreamModel implements model.ChatModel for testing streaming.
type mockStreamModel struct {
	chunks []string
	delay  time.Duration
}

func (m *mockStreamModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return nil, nil
}

func (m *mockStreamModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	sr, sw := schema.Pipe[*schema.Message](len(m.chunks) + 1)
	go func() {
		defer sw.Close()
		for _, c := range m.chunks {
			if m.delay > 0 {
				time.Sleep(m.delay)
			}
			sw.Send(&schema.Message{Role: schema.Assistant, Content: c}, nil)
		}
	}()
	return sr, nil
}

func (m *mockStreamModel) BindTools(_ []*schema.ToolInfo) error {
	return nil
}

func TestStreamCollectWithCallbackThrottling(t *testing.T) {
	// 20 chunks sent with 10ms delay each = ~200ms total.
	// With 200ms throttle, we expect 1-2 throttled callbacks + 1 final = 2-3 total.
	chunks := make([]string, 20)
	for i := range chunks {
		chunks[i] = "x"
	}

	mock := &mockStreamModel{chunks: chunks, delay: 10 * time.Millisecond}

	var callCount atomic.Int64
	var lastText string
	cb := func(textSoFar string) {
		callCount.Add(1)
		lastText = textSoFar
	}

	msg, err := StreamCollectWithCallback(context.Background(), mock, nil, cb)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if msg == nil {
		t.Fatal("expected non-nil message")
	}

	count := callCount.Load()
	// Should have at least 1 (the final callback) and at most ~5 given timing.
	if count < 1 {
		t.Errorf("expected at least 1 callback, got %d", count)
	}
	if count > 10 {
		t.Errorf("expected throttled callbacks (<=10), got %d", count)
	}

	// Final callback should have all text.
	if lastText != "xxxxxxxxxxxxxxxxxxxx" {
		t.Errorf("final text = %q, want 20 x's", lastText)
	}
}

func TestStreamCollectWithCallbackNil(t *testing.T) {
	mock := &mockStreamModel{chunks: []string{"hello", " world"}}

	msg, err := StreamCollectWithCallback(context.Background(), mock, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Content != "hello world" {
		t.Errorf("content = %q, want %q", msg.Content, "hello world")
	}
}

func TestStreamCollectBackwardCompat(t *testing.T) {
	mock := &mockStreamModel{chunks: []string{"a", "b", "c"}}

	msg, err := StreamCollect(context.Background(), mock, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Content != "abc" {
		t.Errorf("content = %q, want %q", msg.Content, "abc")
	}
}
