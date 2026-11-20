package sync

// test_helpers_test.go provides shared test helper functions for the merged
// sync package's engine, observer, planner, and executor tests.

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	stdsync "sync"
	"testing"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncstoretest"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// makeReadOnlyForTest takes the owner's write access from dir, so nothing in
// it can be removed, and gives it back when the test ends. The folder stays
// readable and searchable.
func makeReadOnlyForTest(t *testing.T, dir string) {
	t.Helper()

	syncfixture.SetTestDirPermissions(t, dir, 0o500)
	t.Cleanup(func() { syncfixture.SetTestDirPermissions(t, dir, 0o700) })
}

func schemaFingerprint(tb testing.TB, db *sql.DB) []string {
	tb.Helper()

	rows, err := db.QueryContext(tb.Context(), `
		SELECT type, name, IFNULL(sql, '')
		FROM sqlite_schema
		WHERE type IN ('table', 'index', 'trigger', 'view')
		ORDER BY type, name
	`)
	require.NoError(tb, err)
	defer func() {
		require.NoError(tb, rows.Close())
	}()

	var result []string
	for rows.Next() {
		var itemType string
		var name string
		var ddl string
		require.NoError(tb, rows.Scan(&itemType, &name, &ddl))
		result = append(result, fmt.Sprintf("%s|%s|%s", itemType, name, ddl))
	}
	require.NoError(tb, rows.Err())

	return result
}

func journalMode(tb testing.TB, db *sql.DB) string {
	tb.Helper()

	var mode string
	require.NoError(tb, db.QueryRowContext(tb.Context(), "PRAGMA journal_mode").Scan(&mode))

	return mode
}

func TestNewTestStore_TemplateMatchesFreshStore(t *testing.T) {
	t.Parallel()

	freshPath := filepath.Join(t.TempDir(), "fresh.db")
	fresh, err := syncstore.NewSyncStore(t.Context(), freshPath, syncfixture.NewTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, fresh.Close(context.Background()))
	})

	templateBacked := syncstoretest.NewTestStore(t)

	assert.Equal(t, schemaFingerprint(t, fresh.RawDBForTest()), schemaFingerprint(t, templateBacked.RawDBForTest()))
	assert.Equal(t, journalMode(t, fresh.RawDBForTest()), journalMode(t, templateBacked.RawDBForTest()))
}

type signalingWatcher struct {
	events   chan fsnotify.Event
	errs     chan error
	addOne   stdsync.Once
	addCh    chan struct{}
	closeOne stdsync.Once
}

func newSignalingWatcher() *signalingWatcher {
	return &signalingWatcher{
		events: make(chan fsnotify.Event, 10),
		errs:   make(chan error, 10),
		addCh:  make(chan struct{}),
	}
}

func (w *signalingWatcher) Add(string) error {
	w.addOne.Do(func() { close(w.addCh) })
	return nil
}

func (w *signalingWatcher) Remove(string) error           { return nil }
func (w *signalingWatcher) Events() <-chan fsnotify.Event { return w.events }
func (w *signalingWatcher) Errors() <-chan error          { return w.errs }
func (w *signalingWatcher) Added() <-chan struct{}        { return w.addCh }

func (w *signalingWatcher) Close() error {
	w.closeOne.Do(func() { close(w.events); close(w.errs) })
	return nil
}
