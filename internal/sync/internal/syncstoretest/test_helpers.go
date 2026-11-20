package syncstoretest

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	stdsync "sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

func SaveObservationCursorForTest(t *testing.T, mgr *syncstore.SyncStore, ctx context.Context, driveID string, cursor string) {
	t.Helper()
	require.NoError(t, mgr.CommitObservationCursor(ctx, driveid.New(driveID), cursor))
}

func ReadObservationCursorForTest(t *testing.T, mgr *syncstore.SyncStore, ctx context.Context, expectedDriveID string) string {
	t.Helper()

	state, err := mgr.ReadObservationState(ctx)
	require.NoError(t, err)
	if expectedDriveID != "" && (!state.ContentDriveID.IsZero() || state.Cursor != "") {
		require.Equal(t, driveid.New(expectedDriveID).String(), state.ContentDriveID.String())
	}

	return state.Cursor
}

//nolint:gochecknoglobals // sync tests share one schema template cache to amortize repeated store initialization cost.
var (
	testStoreTemplateOnce stdsync.Once
	testStoreTemplateData []byte
	errTestStoreTemplate  error
)

func NewTestStore(tb testing.TB) *syncstore.SyncStore {
	tb.Helper()

	dbPath := filepath.Join(tb.TempDir(), "test.db")
	ctx := synctest.TestContext(tb)
	require.NoError(tb, os.WriteFile(dbPath, LoadTestStoreTemplate(tb), 0o600), "WriteFile(%q)", dbPath)

	mgr, err := syncstore.OpenSyncStoreForTest(ctx, dbPath, synctest.TestLogger(tb), false)
	require.NoError(tb, err, "NewSyncStore(%q)", dbPath)

	tb.Cleanup(func() {
		assert.NoError(tb, mgr.Close(context.Background()), "Close()")
	})

	return mgr
}

// NewTestManager creates a SyncStore backed by a temp directory for use in
// engine tests that need database access (shared storage, etc.).
func NewTestManager(t *testing.T) *syncstore.SyncStore {
	t.Helper()

	ctx := synctest.TestContext(t)
	dbPath := filepath.Join(t.TempDir(), "test.db")
	require.NoError(t, os.WriteFile(dbPath, LoadTestStoreTemplate(t), 0o600), "WriteFile(%q)", dbPath)

	mgr, err := syncstore.OpenSyncStoreForTest(ctx, dbPath, synctest.TestLogger(t), false)
	require.NoError(t, err, "NewSyncStore(%q)", dbPath)

	t.Cleanup(func() {
		assert.NoError(t, mgr.Close(context.Background()), "Close()")
	})

	return mgr
}

func LoadTestStoreTemplate(tb testing.TB) []byte {
	tb.Helper()

	testStoreTemplateOnce.Do(func() {
		templateDir, err := os.MkdirTemp("", "sync-store-template-")
		if err != nil {
			errTestStoreTemplate = fmt.Errorf("create sync store template dir: %w", err)
			return
		}
		defer os.RemoveAll(templateDir) //nolint:errcheck // a leftover template directory under the test temp root costs nothing

		templatePath := filepath.Join(templateDir, "template.db")
		store, err := syncstore.NewSyncStore(context.Background(), templatePath, slog.New(slog.DiscardHandler))
		if err != nil {
			errTestStoreTemplate = fmt.Errorf("create sync store template: %w", err)
			return
		}
		if closeErr := store.Close(context.Background()); closeErr != nil {
			errTestStoreTemplate = fmt.Errorf("close sync store template: %w", closeErr)
			return
		}

		//nolint:gosec // templatePath is created by this helper inside a private temp dir.
		testStoreTemplateData, err = os.ReadFile(templatePath)
		if err != nil {
			errTestStoreTemplate = fmt.Errorf("read sync store template: %w", err)
		}
	})

	require.NoError(tb, errTestStoreTemplate)

	return append([]byte(nil), testStoreTemplateData...)
}
