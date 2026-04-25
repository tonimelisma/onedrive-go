package sync

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

type fakeDirtyDebounceTimer struct {
	mu         sync.Mutex
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

const dirtyBufferTestTimeout = time.Second

func (t *fakeDirtyDebounceTimer) Chan() <-chan time.Time { return t.ch }
func (t *fakeDirtyDebounceTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	wasActive := t.active
	t.active = false
	return wasActive
}

func (t *fakeDirtyDebounceTimer) Reset(time.Duration) bool {
	t.mu.Lock()
	wasActive := t.active
	t.active = true
	t.resetCount++
	t.mu.Unlock()
	t.resetCh <- struct{}{}
	return wasActive
}

func (t *fakeDirtyDebounceTimer) Fire() {
	t.mu.Lock()
	if !t.active {
		t.mu.Unlock()
		return
	}
	t.active = false
	t.mu.Unlock()
	t.ch <- time.Now()
}

func (t *fakeDirtyDebounceTimer) resetCountSnapshot() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.resetCount
}

// Validates: R-2.1.2
func TestDirtyBuffer_FlushImmediate_CoalescesDirtySignalAndPreservesFullRefresh(t *testing.T) {
	t.Parallel()

	buf := NewDirtyBuffer(synctest.TestLogger(t))
	buf.MarkDirty()
	buf.MarkDirty()
	buf.MarkDirty()
	buf.MarkFullRefresh()

	batch := buf.FlushImmediate()
	require.NotNil(t, batch)
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

	buf.MarkDirty()
	buf.MarkDirty()
	select {
	case <-timer.resetCh:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for debounce reset")
	}
	resetCount := timer.resetCountSnapshot()
	assert.GreaterOrEqual(t, resetCount, 1)
	timer.Fire()

	select {
	case batch := <-out:
		assert.False(t, batch.FullRefresh)
	case <-time.After(dirtyBufferTestTimeout):
		require.Fail(t, "timed out waiting for dirty batch")
	}
}

// Validates: R-2.1.2
func TestDirtyBuffer_FlushDebounced_CoalescesMultipleDirtySignalsIntoOneBatch(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	buf := NewDirtyBuffer(synctest.TestLogger(t))
	timer := newFakeDirtyDebounceTimer()
	buf.newTimer = func(time.Duration) debounceTimer { return timer }
	out := buf.FlushDebounced(ctx, time.Second)

	buf.MarkDirty()
	buf.MarkDirty()
	buf.MarkFullRefresh()

	select {
	case <-timer.resetCh:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for debounce reset")
	}

	resetCount := timer.resetCountSnapshot()
	assert.GreaterOrEqual(t, resetCount, 1)

	timer.Fire()

	select {
	case batch := <-out:
		assert.True(t, batch.FullRefresh)
	case <-time.After(dirtyBufferTestTimeout):
		require.Fail(t, "timed out waiting for debounced dirty batch")
	}

	select {
	case extra := <-out:
		require.Failf(t, "expected one coalesced dirty batch", "unexpected extra batch: %#v", extra)
	default:
	}
}
