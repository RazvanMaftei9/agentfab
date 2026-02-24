package agent

import "fmt"

type CircuitBreaker struct {
	threshold int
	count     int
	tripped   bool
}

func NewCircuitBreaker(threshold int) *CircuitBreaker {
	if threshold <= 0 {
		threshold = 3
	}
	return &CircuitBreaker{threshold: threshold}
}

func (cb *CircuitBreaker) RecordSuccess() {
	cb.count = 0
}

// RecordError increments errors; returns an error if the breaker trips.
func (cb *CircuitBreaker) RecordError() error {
	cb.count++
	if cb.count >= cb.threshold {
		cb.tripped = true
		return fmt.Errorf("circuit breaker tripped after %d consecutive errors", cb.count)
	}
	return nil
}

func (cb *CircuitBreaker) IsTripped() bool {
	return cb.tripped
}
