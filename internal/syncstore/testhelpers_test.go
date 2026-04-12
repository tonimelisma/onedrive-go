package syncstore

// Local test helpers for the syncstore package.
//
// These replicate functionality from the synctest package but are defined here
// to avoid an import cycle: synctest imports syncstore, so syncstore test files
// (which use package syncstore to access unexported symbols) cannot import synctest.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// testDriveID is a canonical test drive ID for all syncstore tests.
const testDriveID = synctest.TestDriveID

func newTestLogger(tb testing.TB) *slog.Logger {
	tb.Helper()
	return synctest.TestLogger(tb)
}

func testContext(tb testing.TB) context.Context {
	tb.Helper()
	return synctest.TestContext(tb)
}

// newTestStore creates a SyncStore backed by a temp directory,
// registering cleanup with t.Cleanup. Does not import synctest to avoid a
// cycle (synctest → syncstore → synctest).
func newTestStore(tb testing.TB) *SyncStore {
	tb.Helper()

	dbPath := filepath.Join(tb.TempDir(), "test.db")
	logger := newTestLogger(tb)

	ctx := testContext(tb)

	mgr, err := NewSyncStore(ctx, dbPath, logger)
	require.NoError(tb, err, "NewSyncStore(%q)", dbPath)

	tb.Cleanup(func() {
		assert.NoError(tb, mgr.Close(ctx), "Close()")
	})

	return mgr
}

func resetInProgressStates(tb testing.TB, mgr *SyncStore, syncRoot string, delayFn func(int) time.Duration) {
	tb.Helper()

	tree, err := synctree.Open(syncRoot)
	require.NoError(tb, err, "synctree.Open(%q)", syncRoot)

	ctx := testContext(tb)
	logger := newTestLogger(tb)

	require.NoError(tb, mgr.ResetDownloadingStates(ctx, delayFn))

	candidates, err := mgr.ListDeletingCandidates(ctx)
	require.NoError(tb, err)

	var (
		deleted []RecoveryCandidate
		pending []RecoveryCandidate
	)

	for _, candidate := range candidates {
		relPath := strings.TrimPrefix(candidate.Path, "/")
		_, statErr := tree.Stat(relPath)
		switch {
		case errors.Is(statErr, os.ErrNotExist):
			deleted = append(deleted, candidate)
		case statErr != nil:
			logger.Warn("crash recovery delete stat failed; retrying delete",
				slog.String("path", candidate.Path),
				slog.String("error", statErr.Error()),
			)
			pending = append(pending, candidate)
		default:
			pending = append(pending, candidate)
		}
	}

	require.NoError(tb, mgr.FinalizeDeletingStates(ctx, deleted, pending, delayFn))
}
