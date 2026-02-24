package runtime

import (
	"context"
	"testing"
)

func TestContextRequestID(t *testing.T) {
	ctx := context.Background()
	if got := RequestIDFrom(ctx); got != "" {
		t.Errorf("RequestIDFrom(empty) = %q, want empty", got)
	}

	ctx = WithRequestID(ctx, "req-123")
	if got := RequestIDFrom(ctx); got != "req-123" {
		t.Errorf("RequestIDFrom = %q, want req-123", got)
	}
}

func TestContextTaskID(t *testing.T) {
	ctx := context.Background()
	if got := TaskIDFrom(ctx); got != "" {
		t.Errorf("TaskIDFrom(empty) = %q, want empty", got)
	}

	ctx = WithTaskID(ctx, "t1")
	if got := TaskIDFrom(ctx); got != "t1" {
		t.Errorf("TaskIDFrom = %q, want t1", got)
	}
}

func TestContextLoopIDAndState(t *testing.T) {
	ctx := context.Background()
	ctx = WithLoopID(ctx, "loop-1")
	ctx = WithLoopState(ctx, "WORKING")

	if got := LoopIDFrom(ctx); got != "loop-1" {
		t.Errorf("LoopIDFrom = %q, want loop-1", got)
	}
	if got := LoopStateFrom(ctx); got != "WORKING" {
		t.Errorf("LoopStateFrom = %q, want WORKING", got)
	}
}

func TestContextStacking(t *testing.T) {
	ctx := context.Background()
	ctx = WithRequestID(ctx, "req-1")
	ctx = WithTaskID(ctx, "t1")
	ctx = WithLoopID(ctx, "loop-1")
	ctx = WithLoopState(ctx, "REVIEWING")

	if got := RequestIDFrom(ctx); got != "req-1" {
		t.Errorf("RequestIDFrom = %q", got)
	}
	if got := TaskIDFrom(ctx); got != "t1" {
		t.Errorf("TaskIDFrom = %q", got)
	}
	if got := LoopIDFrom(ctx); got != "loop-1" {
		t.Errorf("LoopIDFrom = %q", got)
	}
	if got := LoopStateFrom(ctx); got != "REVIEWING" {
		t.Errorf("LoopStateFrom = %q", got)
	}
}
