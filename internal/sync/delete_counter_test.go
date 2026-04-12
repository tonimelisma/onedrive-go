package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// Validates: R-2.3.5
func TestDeleteCounter_BelowThreshold(t *testing.T) {
	t.Parallel()

	now := time.Now()
	c := NewDeleteCounter(10, 5*time.Minute, func() time.Time { return now })

	tripped := c.Add(5)
	assert.False(t, tripped, "should not trip below threshold")
	assert.False(t, c.IsHeld())
	assert.Equal(t, 5, c.Count())
}

// Validates: R-2.3.5
func TestDeleteCounter_CrossingThreshold(t *testing.T) {
	t.Parallel()

	now := time.Now()
	c := NewDeleteCounter(10, 5*time.Minute, func() time.Time { return now })

	// Add 5, then 6 more → total 11 > threshold 10.
	tripped := c.Add(5)
	assert.False(t, tripped)

	tripped = c.Add(6)
	assert.True(t, tripped, "should trip when crossing threshold")
	assert.True(t, c.IsHeld())
}

// Validates: R-2.3.5
func TestDeleteCounter_AtThreshold_NoTrip(t *testing.T) {
	t.Parallel()

	now := time.Now()
	c := NewDeleteCounter(10, 5*time.Minute, func() time.Time { return now })

	// Exactly at threshold (10 == 10) → not greater than, so no trip.
	tripped := c.Add(10)
	assert.False(t, tripped, "should not trip at exactly threshold")
	assert.False(t, c.IsHeld())
}

// Validates: R-2.3.5
func TestDeleteCounter_ExpiredEntries(t *testing.T) {
	t.Parallel()

	now := time.Now()
	c := NewDeleteCounter(10, 5*time.Minute, func() time.Time { return now })

	// Add 8 entries.
	c.Add(8)
	assert.Equal(t, 8, c.Count())

	// Advance time past the window.
	now = now.Add(6 * time.Minute)
	assert.Equal(t, 0, c.Count(), "expired entries should be pruned")

	// Adding 8 more after expiry should not trip (8 < 10).
	tripped := c.Add(8)
	assert.False(t, tripped)
	assert.False(t, c.IsHeld())
}

// Validates: R-2.3.5
func TestDeleteCounter_Release(t *testing.T) {
	t.Parallel()

	now := time.Now()
	c := NewDeleteCounter(10, 5*time.Minute, func() time.Time { return now })

	// Trip the counter.
	c.Add(11)
	require.True(t, c.IsHeld())

	// Release clears held state and entries.
	c.Release()
	assert.False(t, c.IsHeld())
	assert.Equal(t, 0, c.Count())
}

// Validates: R-2.3.5
func TestDeleteCounter_ReTrip(t *testing.T) {
	t.Parallel()

	now := time.Now()
	c := NewDeleteCounter(10, 5*time.Minute, func() time.Time { return now })

	// Trip, release, then trip again.
	c.Add(11)
	require.True(t, c.IsHeld())

	c.Release()
	require.False(t, c.IsHeld())

	tripped := c.Add(11)
	assert.True(t, tripped, "should re-trip after release")
	assert.True(t, c.IsHeld())
}

// Validates: R-2.3.5
func TestDeleteCounter_AccumulateAcrossAdds(t *testing.T) {
	t.Parallel()

	now := time.Now()
	c := NewDeleteCounter(10, 5*time.Minute, func() time.Time { return now })

	// Multiple small adds that accumulate past threshold.
	c.Add(3)
	c.Add(3)
	c.Add(3)
	assert.False(t, c.IsHeld(), "9 total should not trip")

	tripped := c.Add(2)
	assert.True(t, tripped, "11 total should trip")
	assert.True(t, c.IsHeld())
}

// Validates: R-2.3.5
func TestDeleteCounter_ThresholdZero_Disabled(t *testing.T) {
	t.Parallel()

	now := time.Now()
	c := NewDeleteCounter(0, 5*time.Minute, func() time.Time { return now })

	tripped := c.Add(99999)
	assert.False(t, tripped, "threshold=0 disables counter")
	assert.False(t, c.IsHeld())
}

// Validates: R-2.3.5
func TestDeleteCounter_AlreadyHeld_NoReTrip(t *testing.T) {
	t.Parallel()

	now := time.Now()
	c := NewDeleteCounter(10, 5*time.Minute, func() time.Time { return now })

	// Trip once.
	tripped := c.Add(11)
	assert.True(t, tripped)

	// Subsequent Add while held returns false (already tripped).
	tripped = c.Add(5)
	assert.False(t, tripped, "should not re-signal when already held")
	assert.True(t, c.IsHeld(), "should still be held")
}

// Validates: R-2.3.5
func TestDeleteCounter_WindowExpiry_NoTrip(t *testing.T) {
	t.Parallel()

	// Simulates: 500 deletes, wait 6 min, 600 deletes. The first batch expires,
	// so only 600 are in the window → below threshold of 1000.
	now := time.Now()
	c := NewDeleteCounter(1000, 5*time.Minute, func() time.Time { return now })

	c.Add(500)
	now = now.Add(6 * time.Minute) // first batch expires

	tripped := c.Add(600)
	assert.False(t, tripped, "only 600 in window, threshold 1000")
	assert.False(t, c.IsHeld())
	assert.Equal(t, 600, c.Count())
}

// Validates: R-6.4.1
func TestIsActionableIssue_DeleteSafetyHeld(t *testing.T) {
	t.Parallel()

	assert.True(t, syncstore.IsActionableIssue(synctypes.IssueDeleteSafetyHeld),
		"delete_safety_held should be an actionable issue type")
}
