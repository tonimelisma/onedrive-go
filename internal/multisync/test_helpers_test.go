package multisync

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	gosync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"

	_ "modernc.org/sqlite"
)

func testCanonicalID(t *testing.T, s string) driveid.CanonicalID {
	t.Helper()

	cid, err := driveid.NewCanonicalID(s)
	require.NoError(t, err)

	return cid
}

func testStandaloneMountIdentity(cid driveid.CanonicalID) MountIdentity {
	return MountIdentity{
		MountID:        cid.String(),
		ProjectionKind: MountProjectionStandalone,
		CanonicalID:    cid,
	}
}

func testShortcutChildAckRef(t *testing.T, bindingItemID string) syncengine.ShortcutChildAckRef {
	t.Helper()

	return testShortcutChildAckRefWithContext(t.Context(), t, bindingItemID)
}

func testShortcutChildAckRefWithContext(
	ctx context.Context,
	t *testing.T,
	bindingItemID string,
) syncengine.ShortcutChildAckRef {
	t.Helper()

	const namespaceID = "personal:owner@example.com"
	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := syncengine.NewSyncStore(ctx, dbPath, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, store.Close(ctx))
	})

	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})
	_, err = db.ExecContext(ctx, `
		INSERT INTO shortcut_roots (
			namespace_id, binding_item_id, relative_local_path, local_alias,
			remote_drive_id, remote_item_id, remote_is_folder, state,
			protected_paths_json
		) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		namespaceID,
		bindingItemID,
		"Shortcuts/"+bindingItemID,
		bindingItemID,
		"remote-drive",
		"remote-root",
		"active",
		`["Shortcuts/`+bindingItemID+`"]`,
	)
	require.NoError(t, err)

	snapshot, err := store.ShortcutChildWorkSnapshot(ctx, namespaceID, t.TempDir())
	require.NoError(t, err)
	require.Len(t, snapshot.RunCommands, 1)
	require.False(t, snapshot.RunCommands[0].AckRef.IsZero())
	return snapshot.RunCommands[0].AckRef
}

// testClock is a manually advanced clock for restart-backoff tests.
type testClock struct {
	mu      gosync.Mutex
	current time.Time
}

func newTestClock() *testClock {
	return &testClock{current: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = c.current.Add(d)
}

// capturedLog is one record seen by logCapture.
type capturedLog struct {
	level   slog.Level
	message string
	attrs   map[string]string
}

// logCapture is a slog.Handler that keeps every record so tests can assert
// on what a user would see at each verbosity.
type logCapture struct {
	mu      gosync.Mutex
	records []capturedLog
}

func newLogCapture() (*logCapture, *slog.Logger) {
	capture := &logCapture{}
	return capture, slog.New(capture)
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

//nolint:gocritic // slog.Handler fixes the by-value Record parameter.
func (c *logCapture) Handle(_ context.Context, record slog.Record) error {
	attrs := make(map[string]string)
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.String()
		return true
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, capturedLog{level: record.Level, message: record.Message, attrs: attrs})
	return nil
}

func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }

func (c *logCapture) WithGroup(string) slog.Handler { return c }

// atLeast returns the records at level or above, which is what a console at
// that level would show.
func (c *logCapture) atLeast(level slog.Level) []capturedLog {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []capturedLog
	for _, record := range c.records {
		if record.level >= level {
			out = append(out, record)
		}
	}
	return out
}
