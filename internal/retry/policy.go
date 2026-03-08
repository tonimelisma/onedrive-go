package retry

import (
	"context"
	"math"
	"math/rand/v2"
	"time"
)

// Policy defines a reusable exponential backoff configuration. All fields are
// immutable after construction — Policy values are safe for concurrent use.
type Policy struct {
	// MaxAttempts is the total number of attempts (including the first).
	// 0 means infinite (for watch loops that never give up).
	MaxAttempts int

	// Base is the initial backoff delay before any multiplication.
	Base time.Duration

	// Max is the ceiling for the computed delay. Backoff never exceeds this.
	Max time.Duration

	// Multiplier is the exponential factor applied per attempt (typically 2.0).
	Multiplier float64

	// Jitter is the fraction of the delay used for randomization (0.0–1.0).
	// A value of 0.25 means ±25% jitter. 0.0 disables jitter.
	Jitter float64
}

// Delay returns the backoff duration for the given attempt (0-indexed).
// The result includes jitter if configured.
func (p Policy) Delay(attempt int) time.Duration {
	backoff := float64(p.Base) * math.Pow(p.Multiplier, float64(attempt))
	if backoff > float64(p.Max) {
		backoff = float64(p.Max)
	}

	if p.Jitter > 0 {
		// ±jitter: e.g., 0.25 jitter means backoff * 0.25 * [-1, 1)
		jitter := backoff * p.Jitter * (rand.Float64()*2 - 1) //nolint:gosec // jitter does not need crypto rand
		backoff += jitter
	}

	return time.Duration(backoff)
}

// SleepFunc is a context-aware sleep function. The default is TimeSleep.
type SleepFunc func(ctx context.Context, d time.Duration) error

// TimeSleep waits for the given duration or until the context is canceled.
func TimeSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
