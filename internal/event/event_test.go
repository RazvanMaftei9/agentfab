package event

import (
	"testing"
	"time"
)

func TestEmitSetsTimestamp(t *testing.T) {
	bus := NewBus()
	defer bus.Close()

	before := time.Now()
	bus.Emit(Event{Type: AgentReady, AgentName: "dev"})
	after := time.Now()

	e := <-bus
	if e.Timestamp.Before(before) || e.Timestamp.After(after) {
		t.Errorf("timestamp %v not in [%v, %v]", e.Timestamp, before, after)
	}
}

func TestEmitPreservesExplicitTimestamp(t *testing.T) {
	bus := NewBus()
	defer bus.Close()

	ts := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	bus.Emit(Event{Type: AgentReady, Timestamp: ts})

	e := <-bus
	if !e.Timestamp.Equal(ts) {
		t.Errorf("timestamp: got %v, want %v", e.Timestamp, ts)
	}
}

func TestEmitNonBlockingWhenFull(t *testing.T) {
	bus := make(Bus, 1)

	bus.Emit(Event{Type: AgentReady})
	// Buffer is full — this should not block.
	bus.Emit(Event{Type: AllAgentsReady})

	e := <-bus
	if e.Type != AgentReady {
		t.Errorf("type: got %d, want %d", e.Type, AgentReady)
	}

	// Channel should be empty now (second event was dropped).
	select {
	case <-bus:
		t.Error("expected empty channel")
	default:
	}
}

func TestCloseExitsRangeLoop(t *testing.T) {
	bus := NewBus()
	bus.Emit(Event{Type: AgentReady})
	bus.Close()

	count := 0
	for range bus {
		count++
	}
	if count != 1 {
		t.Errorf("count: got %d, want 1", count)
	}
}

func TestNilBusSafe(t *testing.T) {
	var bus Bus
	// Should not panic.
	bus.Emit(Event{Type: AgentReady})
	bus.Close()
}
