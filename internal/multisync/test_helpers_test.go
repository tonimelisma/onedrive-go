package multisync

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"

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
