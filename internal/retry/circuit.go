package retry

import (
	"errors"
	"sync"
	"time"
)

// ErrCircuitOpen is returned when the circuit breaker is open and rejecting
// requests. Callers should treat this as a transient error and retry later.
var ErrCircuitOpen = errors.New("retry: circuit breaker is open")

// CircuitState represents the current state of a circuit breaker.
type CircuitState int

const (
	// CircuitClosed is the normal operating state. Requests flow through.
	CircuitClosed CircuitState = iota
	// CircuitOpen means too many failures occurred. Requests are rejected.
	CircuitOpen
	// CircuitHalfOpen allows a single probe request to test recovery.
	CircuitHalfOpen
)

// String returns a human-readable name for the circuit state.
func (s CircuitState) String() string {
	switch s {
	case CircuitClosed:
		return "closed"
	case CircuitOpen:
		return "open"
	case CircuitHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// CircuitBreaker implements the circuit breaker pattern for detecting and
// short-circuiting during service-wide outages. Thread-safe.
//
// State transitions:
//
//	Closed → Open:     failures within window >= threshold
//	Open → HalfOpen:   cooldown elapsed since breaker opened
//	HalfOpen → Closed: probe request succeeds
//	HalfOpen → Open:   probe request fails
type CircuitBreaker struct {
	mu               sync.Mutex
	failureThreshold int
	window           time.Duration
	cooldown         time.Duration
	state            CircuitState
	failures         []time.Time // timestamps of failures within the window
	openedAt         time.Time
	nowFunc          func() time.Time // injectable for tests
}

// NewCircuitBreaker creates a circuit breaker.
//   - threshold: number of failures within window to trip the breaker.
//   - window: sliding time window for counting failures.
//   - cooldown: time in open state before allowing a half-open probe.
func NewCircuitBreaker(threshold int, window, cooldown time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		failureThreshold: threshold,
		window:           window,
		cooldown:         cooldown,
		nowFunc:          time.Now,
	}
}

// Allow reports whether a request should proceed. Returns true if the circuit
// is closed or if it's half-open and a probe is allowed. Returns false when
// the circuit is open and the cooldown hasn't elapsed.
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := cb.nowFunc()

	switch cb.state {
	case CircuitClosed:
		return true
	case CircuitOpen:
		if now.Sub(cb.openedAt) >= cb.cooldown {
			cb.state = CircuitHalfOpen
			return true
		}

		return false
	case CircuitHalfOpen:
		// Only one probe at a time — reject additional requests while probing.
		return false
	default:
		return true
	}
}

// RecordSuccess records a successful request. Resets the breaker to closed.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.state = CircuitClosed
	cb.failures = cb.failures[:0]
}

// RecordFailure records a failed request. If failures within the window reach
// the threshold, the breaker trips open.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := cb.nowFunc()

	if cb.state == CircuitHalfOpen {
		// Probe failed — back to open.
		cb.state = CircuitOpen
		cb.openedAt = now

		return
	}

	// Prune failures outside the window.
	cutoff := now.Add(-cb.window)
	pruned := cb.failures[:0]

	for _, t := range cb.failures {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}

	pruned = append(pruned, now)
	cb.failures = pruned

	if len(cb.failures) >= cb.failureThreshold {
		cb.state = CircuitOpen
		cb.openedAt = now
	}
}

// State returns the current circuit state. Thread-safe.
func (cb *CircuitBreaker) State() CircuitState {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	// Check for automatic transition from open to half-open.
	if cb.state == CircuitOpen {
		now := cb.nowFunc()
		if now.Sub(cb.openedAt) >= cb.cooldown {
			cb.state = CircuitHalfOpen
		}
	}

	return cb.state
}
