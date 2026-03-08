package retry

import "time"

// Backoff tracks the current backoff level for long-running loops (observer
// watch loops). It wraps a Policy with mutable state: the current consecutive
// error count. Not safe for concurrent use — intended for single-goroutine
// watch loops.
type Backoff struct {
	policy      Policy
	consecutive int
	maxOverride time.Duration // dynamic max (e.g., poll interval for remote observer)
}

// NewBackoff creates a Backoff from a Policy. The initial state is zero
// consecutive errors (first call to Next returns the base delay).
func NewBackoff(p Policy) *Backoff {
	return &Backoff{policy: p}
}

// Next returns the current backoff delay and advances the consecutive error
// count for the next call. This matches the remote observer's "use then
// advance" pattern: sleep with the returned delay, then call Next again on
// the next error.
func (b *Backoff) Next() time.Duration {
	p := b.policy
	if b.maxOverride > 0 {
		p.Max = b.maxOverride
	}

	delay := p.Delay(b.consecutive)
	b.consecutive++

	return delay
}

// Reset sets the consecutive error count to zero. Call on success.
func (b *Backoff) Reset() {
	b.consecutive = 0
}

// SetMaxOverride sets a dynamic maximum backoff duration that overrides the
// policy's Max. Used by the remote observer to cap backoff at the poll
// interval. Pass 0 to revert to the policy default.
func (b *Backoff) SetMaxOverride(d time.Duration) {
	b.maxOverride = d
}

// Consecutive returns the current consecutive error count.
func (b *Backoff) Consecutive() int {
	return b.consecutive
}
