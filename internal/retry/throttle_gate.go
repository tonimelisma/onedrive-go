package retry

import (
	"context"
	"sync"
	"time"
)

// ThrottleGate coordinates shared Retry-After deadlines across multiple retry
// transports that intentionally belong to the same caller scope.
type ThrottleGate struct {
	mu       sync.Mutex
	deadline time.Time
}

// Wait blocks until the current throttle deadline passes.
func (g *ThrottleGate) Wait(ctx context.Context, sleepFn SleepFunc) error {
	if g == nil {
		return nil
	}

	g.mu.Lock()
	deadline := g.deadline
	g.mu.Unlock()

	if delay := time.Until(deadline); delay > 0 {
		return sleepFn(ctx, delay)
	}

	return nil
}

// SetDeadline stores the latest throttle deadline.
func (g *ThrottleGate) SetDeadline(deadline time.Time) {
	if g == nil {
		return
	}

	g.mu.Lock()
	if deadline.After(g.deadline) {
		g.deadline = deadline
	}
	g.mu.Unlock()
}

// Deadline returns the currently stored throttle deadline.
func (g *ThrottleGate) Deadline() time.Time {
	if g == nil {
		return time.Time{}
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	return g.deadline
}
