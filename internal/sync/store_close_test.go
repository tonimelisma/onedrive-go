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

// Regression: a clean Ctrl-C closed the store with the already-canceled
// shutdown context, so the WAL checkpoint failed and a false "checkpoint
// failed on close" warning was printed on every stop.
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
	assert.NotContains(t, logs.String(), "checkpoint failed on close")
}
