package agent

import "testing"

func TestCircuitBreakerTrips(t *testing.T) {
	cb := NewCircuitBreaker(3)

	cb.RecordError()
	cb.RecordError()
	if cb.IsTripped() {
		t.Fatal("should not trip at 2 errors")
	}

	err := cb.RecordError()
	if err == nil {
		t.Fatal("expected error on 3rd consecutive error")
	}
	if !cb.IsTripped() {
		t.Fatal("should be tripped")
	}
}

func TestCircuitBreakerResets(t *testing.T) {
	cb := NewCircuitBreaker(3)

	cb.RecordError()
	cb.RecordError()
	cb.RecordSuccess() // Reset.
	cb.RecordError()
	cb.RecordError()

	if cb.IsTripped() {
		t.Fatal("should not trip — success reset the counter")
	}
}
