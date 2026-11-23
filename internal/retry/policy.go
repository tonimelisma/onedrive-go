package retry

import (
	"context"
	"fmt"
	"math"
	"time"
)

// Policy defines a reusable exponential backoff configuration. All fields are
// immutable after construction — Policy values are safe for concurrent use.
type Policy struct {
	// MaxAttempts is the total number of attempts (including the first), for
	// every consumer: RetryTransport, the graph quirk retries, and the
	// driveops visibility waits. 0 means infinite (for watch loops that
	// never give up); RetryTransport treats 0 and 1 alike as "no retry".
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

// Delay returns the backoff duration for the given attempt (0-indexed). A
// policy with Jitter draws its spread from jitter, the source the caller
// passes: ±Jitter of the backoff, so 0.25 gives backoff * [0.75, 1.25). A
// nil source draws nothing and the delay is the curve itself, as for a
// policy without Jitter.
func (p Policy) Delay(attempt int, jitter JitterSource) time.Duration {
	backoff := float64(p.Base) * math.Pow(p.Multiplier, float64(attempt))
	if backoff > float64(p.Max) {
		backoff = float64(p.Max)
	}

	if p.Jitter > 0 && jitter != nil {
		spread := backoff * p.Jitter
		backoff += float64(jitter.Jitter(time.Duration(2*spread))) - spread
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
		return fmt.Errorf("sleep context: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}
