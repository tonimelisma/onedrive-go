package retry

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestBackoff_Next_IncreasesDelay(t *testing.T) {
	t.Parallel()

	b := NewBackoff(WatchLocal) // 1s base, 30s max, 2x, no jitter

	assert.Equal(t, 1*time.Second, b.Next())
	assert.Equal(t, 1, b.Consecutive())

	assert.Equal(t, 2*time.Second, b.Next())
	assert.Equal(t, 2, b.Consecutive())

	assert.Equal(t, 4*time.Second, b.Next())
	assert.Equal(t, 3, b.Consecutive())
}

func TestBackoff_Reset(t *testing.T) {
	t.Parallel()

	b := NewBackoff(WatchLocal)

	b.Next()
	b.Next()
	assert.Equal(t, 2, b.Consecutive())

	b.Reset()
	assert.Equal(t, 0, b.Consecutive())
	assert.Equal(t, 1*time.Second, b.Next()) // back to base
}

func TestBackoff_MaxCap(t *testing.T) {
	t.Parallel()

	b := NewBackoff(WatchLocal) // max 30s

	// Advance past max.
	for range 10 {
		b.Next()
	}

	// Should be capped at 30s.
	assert.Equal(t, 30*time.Second, b.Next())
}

func TestBackoff_SetMaxOverride(t *testing.T) {
	t.Parallel()

	b := NewBackoff(WatchRemote) // 5s base, 5m default max, no jitter

	// Without override: 5s * 2^4 = 80s < 5m, so no cap.
	for range 4 {
		b.Next()
	}
	assert.Equal(t, 80*time.Second, b.Next())

	// Set override to 30s (typical poll interval).
	b.Reset()
	b.SetMaxOverride(30 * time.Second)

	assert.Equal(t, 5*time.Second, b.Next())  // 5s * 2^0
	assert.Equal(t, 10*time.Second, b.Next()) // 5s * 2^1
	assert.Equal(t, 20*time.Second, b.Next()) // 5s * 2^2
	assert.Equal(t, 30*time.Second, b.Next()) // capped at override (40s > 30s)
	assert.Equal(t, 30*time.Second, b.Next()) // still capped
}

func TestBackoff_RemoteObserverPattern(t *testing.T) {
	t.Parallel()

	// Simulate the remote observer's "use then advance" pattern:
	// 1. Sleep with current backoff
	// 2. Advance backoff for next error
	// This is exactly what Next() does.
	b := NewBackoff(WatchRemote)
	b.SetMaxOverride(60 * time.Second) // poll interval

	// First error: sleep 5s, advance.
	d1 := b.Next()
	assert.Equal(t, 5*time.Second, d1)

	// Second error: sleep 10s, advance.
	d2 := b.Next()
	assert.Equal(t, 10*time.Second, d2)

	// Success resets.
	b.Reset()
	d3 := b.Next()
	assert.Equal(t, 5*time.Second, d3) // back to base
}
