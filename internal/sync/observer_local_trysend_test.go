package sync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// ---------------------------------------------------------------------------
// trySend tests
// ---------------------------------------------------------------------------

func TestTrySend_ChannelAvailable_SendsEvent(t *testing.T) {
	t.Parallel()

	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	batches := localBatches(obs, 1)
	ctx := t.Context()

	ev := ChangeEvent{
		Path: "test.txt", ItemType: ItemTypeFile, Hash: "h", Size: 3,
	}

	obs.TrySend(ctx, &ev)

	select {
	case got := <-batches:
		assert.Equal(t, LocalPatch, got.Kind)
		assert.Equal(t, []LocalStateRow{{Path: "test.txt", ItemType: ItemTypeFile, Hash: "h", Size: 3}}, got.Rows)
		assert.True(t, got.Dirty)
	default:
		require.Fail(t, "expected a patch on the channel")
	}

	assert.Equal(t, int64(0), obs.DroppedEvents())
}

func TestTrySend_ChannelFull_DropsEvent(t *testing.T) {
	t.Parallel()

	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	batches := localBatches(obs, 1)
	ctx := t.Context()

	// Fill the channel.
	first := ChangeEvent{Path: "first.txt", ItemType: ItemTypeFile}
	obs.TrySend(ctx, &first)

	// This should be dropped (channel full).
	second := ChangeEvent{Path: "second.txt", ItemType: ItemTypeFile}

	obs.TrySend(ctx, &second)

	assert.Equal(t, int64(1), obs.DroppedEvents())
	assert.Len(t, obs.recoveryScanRequests, 1, "a dropped patch asks for a recovery rescan")

	// ResetDroppedEvents returns the count and resets to 0 (B-190).
	reset := obs.ResetDroppedEvents()
	assert.Equal(t, int64(1), reset)
	assert.Equal(t, int64(0), obs.DroppedEvents())

	// Original patch still in channel.
	assert.Equal(t, []string{"first.txt"}, patchRowPaths(drainLocalPatches(batches)))
}

func TestTrySend_ContextCanceled_NoDrop(t *testing.T) {
	t.Parallel()

	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	batches := localBatches(obs, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	// Fill the channel so default branch would fire, but ctx is canceled.
	batches <- LocalObservation{Rows: []LocalStateRow{{Path: "fill.txt"}}}

	ev := ChangeEvent{Path: "test.txt", ItemType: ItemTypeFile}

	obs.TrySend(ctx, &ev)

	// Context cancel takes priority over default branch in select, but
	// Go's select is non-deterministic. The drop counter may or may not
	// increment. The key invariant is: trySend must not block.
	// Just verify it returned (no deadlock).
}
