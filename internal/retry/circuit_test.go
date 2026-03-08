package retry

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestBreaker(threshold int, window, cooldown time.Duration) (*CircuitBreaker, *time.Time) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	cb := NewCircuitBreaker(threshold, window, cooldown)
	cb.nowFunc = func() time.Time { return now }

	return cb, &now
}

func TestCircuitBreaker_StartsClosedAllowsRequests(t *testing.T) {
	t.Parallel()

	cb, _ := newTestBreaker(5, 30*time.Second, 60*time.Second)

	assert.Equal(t, CircuitClosed, cb.State())
	assert.True(t, cb.Allow())
}

func TestCircuitBreaker_TripsOpenOnThreshold(t *testing.T) {
	t.Parallel()

	cb, _ := newTestBreaker(3, 30*time.Second, 60*time.Second)

	cb.RecordFailure()
	cb.RecordFailure()
	assert.Equal(t, CircuitClosed, cb.State())
	assert.True(t, cb.Allow())

	cb.RecordFailure() // 3rd failure = threshold
	assert.Equal(t, CircuitOpen, cb.State())
	assert.False(t, cb.Allow())
}

func TestCircuitBreaker_OpenToHalfOpenAfterCooldown(t *testing.T) {
	t.Parallel()

	cb, now := newTestBreaker(2, 30*time.Second, 60*time.Second)

	cb.RecordFailure()
	cb.RecordFailure()
	assert.Equal(t, CircuitOpen, cb.State())

	// Before cooldown: still open.
	*now = now.Add(59 * time.Second)
	assert.False(t, cb.Allow())

	// After cooldown: transitions to half-open, allows one probe.
	*now = now.Add(2 * time.Second) // now at T+61s
	assert.True(t, cb.Allow())
	assert.Equal(t, CircuitHalfOpen, cb.State())
}

func TestCircuitBreaker_HalfOpenSuccessCloses(t *testing.T) {
	t.Parallel()

	cb, now := newTestBreaker(2, 30*time.Second, 60*time.Second)

	cb.RecordFailure()
	cb.RecordFailure()

	*now = now.Add(61 * time.Second)
	require.True(t, cb.Allow()) // transitions to half-open

	cb.RecordSuccess()
	assert.Equal(t, CircuitClosed, cb.State())
	assert.True(t, cb.Allow())
}

func TestCircuitBreaker_HalfOpenFailureReopens(t *testing.T) {
	t.Parallel()

	cb, now := newTestBreaker(2, 30*time.Second, 60*time.Second)

	cb.RecordFailure()
	cb.RecordFailure()

	*now = now.Add(61 * time.Second)
	require.True(t, cb.Allow()) // transitions to half-open

	cb.RecordFailure()
	assert.Equal(t, CircuitOpen, cb.State())
	assert.False(t, cb.Allow())
}

func TestCircuitBreaker_HalfOpenRejectsSecondProbe(t *testing.T) {
	t.Parallel()

	cb, now := newTestBreaker(2, 30*time.Second, 60*time.Second)

	cb.RecordFailure()
	cb.RecordFailure()

	*now = now.Add(61 * time.Second)
	require.True(t, cb.Allow()) // first probe allowed

	// Second call while in half-open should be rejected.
	assert.False(t, cb.Allow())
}

func TestCircuitBreaker_FailuresOutsideWindowPruned(t *testing.T) {
	t.Parallel()

	cb, now := newTestBreaker(3, 10*time.Second, 30*time.Second)

	// Two failures at T=0.
	cb.RecordFailure()
	cb.RecordFailure()

	// Advance past window.
	*now = now.Add(11 * time.Second)

	// Third failure at T=11. Only this failure is within the window.
	cb.RecordFailure()
	assert.Equal(t, CircuitClosed, cb.State()) // only 1 failure in window, below threshold of 3
}

func TestCircuitBreaker_SuccessResetsClearsFaitures(t *testing.T) {
	t.Parallel()

	cb, _ := newTestBreaker(3, 30*time.Second, 60*time.Second)

	cb.RecordFailure()
	cb.RecordFailure()
	cb.RecordSuccess() // should clear failures
	cb.RecordFailure()

	assert.Equal(t, CircuitClosed, cb.State()) // only 1 failure after reset
}

func TestCircuitState_String(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "closed", CircuitClosed.String())
	assert.Equal(t, "open", CircuitOpen.String())
	assert.Equal(t, "half-open", CircuitHalfOpen.String())
	assert.Equal(t, "unknown", CircuitState(99).String())
}
