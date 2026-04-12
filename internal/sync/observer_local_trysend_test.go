package sync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// ---------------------------------------------------------------------------
// trySend tests
// ---------------------------------------------------------------------------

func TestTrySend_ChannelAvailable_SendsEvent(t *testing.T) {
	t.Parallel()

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	events := make(chan synctypes.ChangeEvent, 1)
	ctx := t.Context()

	ev := synctypes.ChangeEvent{
		Source: synctypes.SourceLocal, Type: synctypes.ChangeCreate, Path: "test.txt",
		ItemType: synctypes.ItemTypeFile,
	}

	obs.TrySend(ctx, events, &ev)

	select {
	case got := <-events:
		assert.Equal(t, "test.txt", got.Path)
	default:
		require.Fail(t, "expected event on channel")
	}

	assert.Equal(t, int64(0), obs.DroppedEvents())
}

func TestTrySend_ChannelFull_DropsEvent(t *testing.T) {
	t.Parallel()

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	events := make(chan synctypes.ChangeEvent, 1)
	ctx := t.Context()

	// Fill the channel.
	first := synctypes.ChangeEvent{
		Source: synctypes.SourceLocal, Type: synctypes.ChangeCreate, Path: "first.txt",
		ItemType: synctypes.ItemTypeFile,
	}
	events <- first

	// This should be dropped (channel full).
	second := synctypes.ChangeEvent{
		Source: synctypes.SourceLocal, Type: synctypes.ChangeCreate, Path: "second.txt",
		ItemType: synctypes.ItemTypeFile,
	}

	obs.TrySend(ctx, events, &second)

	assert.Equal(t, int64(1), obs.DroppedEvents())

	// ResetDroppedEvents returns the count and resets to 0 (B-190).
	reset := obs.ResetDroppedEvents()
	assert.Equal(t, int64(1), reset)
	assert.Equal(t, int64(0), obs.DroppedEvents())

	// Original event still in channel.
	got := <-events
	assert.Equal(t, "first.txt", got.Path)
}

func TestTrySend_ContextCanceled_NoDrop(t *testing.T) {
	t.Parallel()

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	events := make(chan synctypes.ChangeEvent, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	// Fill the channel so default branch would fire, but ctx is canceled.
	events <- synctypes.ChangeEvent{Path: "fill.txt"}

	ev := synctypes.ChangeEvent{
		Source: synctypes.SourceLocal, Type: synctypes.ChangeCreate, Path: "test.txt",
		ItemType: synctypes.ItemTypeFile,
	}

	obs.TrySend(ctx, events, &ev)

	// Context cancel takes priority over default branch in select, but
	// Go's select is non-deterministic. The drop counter may or may not
	// increment. The key invariant is: trySend must not block.
	// Just verify it returned (no deadlock).
}
