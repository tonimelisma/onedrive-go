package sync

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A clean Ctrl-C closes the store with the already-canceled shutdown
// context. The WAL checkpoint runs detached from it, so it still succeeds
// and the close says nothing at Warn.
//
// Validates: R-6.6.21
func TestSyncStore_CloseWithCanceledContextStillCheckpoints(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	store, err := NewSyncStore(t.Context(), filepath.Join(t.TempDir(), "state.db"), logger)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.NoError(t, store.Close(ctx))
	assert.Empty(t, logs.String(), "a clean close says nothing at Warn")
}

// A checkpoint that fails on close is said once at Warn, with the word that
// committed data is unaffected, and the close itself still succeeds.
//
// Validates: R-6.6.21
func TestSyncStore_CloseSaysAFailedCheckpointAtWarn(t *testing.T) {
	t.Parallel()

	logger, logs := newCapturedLogger()
	store, err := NewSyncStore(t.Context(), filepath.Join(t.TempDir(), "state.db"), logger)
	require.NoError(t, err)
	// A checkpoint of a closed database fails.
	require.NoError(t, store.RawDBForTest().Close())

	require.NoError(t, store.Close(t.Context()))

	output := logs.String()
	assert.Equal(t, 1, countLevel(output, "WARN"), output)
	assert.Contains(t, output, "committed data is unaffected")
}

// A second close, as of an engine closed twice, does nothing: it checkpoints
// no closed database and so says nothing at Warn.
//
// Validates: R-6.6.21
func TestSyncStore_SecondCloseDoesNothing(t *testing.T) {
	t.Parallel()

	logger, logs := newCapturedLogger()
	store, err := NewSyncStore(t.Context(), filepath.Join(t.TempDir(), "state.db"), logger)
	require.NoError(t, err)

	require.NoError(t, store.Close(t.Context()))
	require.NoError(t, store.Close(t.Context()))

	assert.Zero(t, countLevel(logs.String(), "WARN"), logs.String())
}
