// delete_counter.go — Rolling-window delete counter for watch-mode big-delete protection.
//
// The counter tracks planned delete actions within a configurable time window.
// When the cumulative count exceeds the threshold, all subsequent deletes are
// held until the user explicitly clears them via `issues clear`.
//
// The counter is purely in-memory — on daemon restart it resets. The one-shot
// planner-level threshold check handles the restart case (RunOnce runs first
// in RunWatch before entering the event loop).
package sync

import (
	stdsync "sync"
	"time"
)

// deleteCounter tracks planned delete actions within a rolling time window.
// Thread-safe — all methods acquire the mutex.
type deleteCounter struct {
	mu        stdsync.Mutex
	entries   []time.Time   // timestamps of planned deletes
	window    time.Duration // rolling window duration (e.g. 5 minutes)
	threshold int           // max deletes before tripping
	held      bool          // latches true on first trip
	nowFunc   func() time.Time
}

// newDeleteCounter creates a deleteCounter. A threshold of 0 disables the
// counter (Add always returns false, IsHeld always returns false).
//
//nolint:unparam // window parameter kept for testability even though production code always passes deleteCounterWindow
func newDeleteCounter(threshold int, window time.Duration, nowFunc func() time.Time) *deleteCounter {
	return &deleteCounter{
		threshold: threshold,
		window:    window,
		nowFunc:   nowFunc,
	}
}

// Add records count new planned deletes at the current time and returns true
// if this call caused the counter to trip (transition from not-held to held).
// Expired entries are pruned before the check.
func (c *deleteCounter) Add(count int) bool {
	if c.threshold <= 0 {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.nowFunc()
	c.expire(now)

	// Record each delete as a separate timestamp entry.
	for range count {
		c.entries = append(c.entries, now)
	}

	// Check if we just crossed the threshold.
	if !c.held && len(c.entries) > c.threshold {
		c.held = true
		return true
	}

	return false
}

// IsHeld returns true if the counter has been tripped and deletes are held.
func (c *deleteCounter) IsHeld() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.held
}

// Release resets the held flag and clears all entries. Called when the user
// approves held deletes via `issues clear`.
func (c *deleteCounter) Release() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.held = false
	c.entries = nil
}

// Count returns the current number of entries in the rolling window.
func (c *deleteCounter) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.expire(c.nowFunc())

	return len(c.entries)
}

// Threshold returns the configured threshold.
func (c *deleteCounter) Threshold() int {
	return c.threshold
}

// expire removes entries older than the window. Must be called with mu held.
func (c *deleteCounter) expire(now time.Time) {
	cutoff := now.Add(-c.window)

	// Entries are appended in time order — find the first non-expired entry.
	i := 0
	for i < len(c.entries) && c.entries[i].Before(cutoff) {
		i++
	}

	if i > 0 {
		c.entries = c.entries[i:]
	}
}
