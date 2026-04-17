package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

type fakeDirtyDebounceTimer struct {
	ch         chan time.Time
	resetCh    chan struct{}
	active     bool
	resetCount int
}

func newFakeDirtyDebounceTimer() *fakeDirtyDebounceTimer {
	return &fakeDirtyDebounceTimer{
		ch:      make(chan time.Time, 1),
		resetCh: make(chan struct{}, 8),
	}
}

func (t *fakeDirtyDebounceTimer) Chan() <-chan time.Time { return t.ch }
func (t *fakeDirtyDebounceTimer) Stop() bool {
	wasActive := t.active
	t.active = false
	return wasActive
}
func (t *fakeDirtyDebounceTimer) Reset(time.Duration) bool {
	wasActive := t.active
	t.active = true
	t.resetCount++
	t.resetCh <- struct{}{}
	return wasActive
}

func (t *fakeDirtyDebounceTimer) Fire() {
	if !t.active {
		return
	}
	t.active = false
	t.ch <- time.Now()
}

// Validates: R-2.1.2
func TestDirtyBuffer_FlushImmediate_DedupesPathsAndPreservesFullRefresh(t *testing.T) {
	t.Parallel()

	buf := NewDirtyBuffer(synctest.TestLogger(t))
	buf.MarkPath("b.txt")
	buf.MarkPath("a.txt")
	buf.MarkPath("a.txt")
	buf.MarkFullRefresh()

	batch := buf.FlushImmediate()
	require.NotNil(t, batch)
	assert.Equal(t, []string{"a.txt", "b.txt"}, batch.Paths)
	assert.True(t, batch.FullRefresh)
	assert.Nil(t, buf.FlushImmediate())
}

// Validates: R-2.1.2
func TestDirtyBuffer_FlushDebounced_UsesLastObservationWindow(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	buf := NewDirtyBuffer(synctest.TestLogger(t))
	timer := newFakeDirtyDebounceTimer()
	buf.newTimer = func(time.Duration) debounceTimer { return timer }
	out := buf.FlushDebounced(ctx, time.Second)

	buf.MarkPath("alpha.txt")
	buf.MarkPath("beta.txt")
	select {
	case <-timer.resetCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for debounce reset")
	}
	assert.GreaterOrEqual(t, timer.resetCount, 1)
	timer.Fire()

	select {
	case batch := <-out:
		assert.Equal(t, []string{"alpha.txt", "beta.txt"}, batch.Paths)
		assert.False(t, batch.FullRefresh)
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for dirty batch")
	}
}
