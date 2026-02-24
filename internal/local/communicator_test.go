package local

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/razvanmaftei/agentfab/internal/message"
)

func TestCommunicatorSendReceive(t *testing.T) {
	hub := NewHub()
	conductor := hub.Register("conductor")
	developer := hub.Register("developer")

	ctx := context.Background()

	msg := &message.Message{
		ID:   "msg-1",
		From: "conductor",
		To:   "developer",
		Type: message.TypeTaskAssignment,
		Parts: []message.Part{
			message.TextPart{Text: "Build the login page"},
		},
		Timestamp: time.Now(),
	}

	if err := conductor.Send(ctx, msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	select {
	case received := <-developer.Receive(ctx):
		if received.ID != msg.ID {
			t.Errorf("ID: got %q, want %q", received.ID, msg.ID)
		}
		if received.From != "conductor" {
			t.Errorf("From: got %q", received.From)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for message")
	}
}

func TestCommunicatorUnknownAgent(t *testing.T) {
	hub := NewHub()
	sender := hub.Register("sender")
	ctx := context.Background()

	err := sender.Send(ctx, &message.Message{To: "nobody"})
	if err == nil {
		t.Fatal("expected error sending to unknown agent")
	}
}

func TestCommunicatorDeregister(t *testing.T) {
	hub := NewHub()
	hub.Register("agent")
	hub.Deregister("agent")

	sender := hub.Register("sender")
	err := sender.Send(context.Background(), &message.Message{To: "agent"})
	if err == nil {
		t.Fatal("expected error after deregister")
	}
}

func TestCommunicatorConcurrentSendDeregister(t *testing.T) {
	hub := NewHub()
	sender := hub.Register("sender")
	hub.Register("target")

	ctx := context.Background()
	var wg sync.WaitGroup

	// Concurrent sends while deregistering — must not panic.
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = sender.Send(ctx, &message.Message{To: "target"})
		}
	}()
	go func() {
		defer wg.Done()
		time.Sleep(time.Microsecond)
		hub.Deregister("target")
	}()
	wg.Wait()
}
