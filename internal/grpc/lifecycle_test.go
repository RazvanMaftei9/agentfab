package grpc

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestProcessLifecycleTeardownAllHonorsContext(t *testing.T) {
	l := &ProcessLifecycle{
		procs: map[string]*agentProcess{
			"architect": {done: make(chan struct{})},
			"designer":  {done: make(chan struct{})},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := l.TeardownAll(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("TeardownAll error = %v, want deadline exceeded", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("TeardownAll took %v, expected it to stop near context deadline", elapsed)
	}
}
