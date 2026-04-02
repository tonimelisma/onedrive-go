package sync

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// ---------------------------------------------------------------------------
// Conflict resolution tests
// ---------------------------------------------------------------------------

// Validates: R-2.3.4
func TestResolveConflict_KeepBoth(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems(nil, "token-1"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	// Create the file on disk — resolveKeepBoth hashes it to update baseline.
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "conflict-file.txt"), []byte("local content"), 0o600))

	// Seed a conflict.
	outcomes := []synctypes.Outcome{{
		Action:       synctypes.ActionConflict,
		Success:      true,
		Path:         "conflict-file.txt",
		DriveID:      driveID,
		ItemID:       "item-c",
		ItemType:     synctypes.ItemTypeFile,
		LocalHash:    "local-h",
		RemoteHash:   "remote-h",
		ConflictType: "edit_edit",
	}}

	seedBaseline(t, eng.baseline, ctx, outcomes, "")

	// Get conflict ID.
	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts")
	require.Len(t, conflicts, 1)

	// Resolve as keep_both.
	require.NoError(t, eng.ResolveConflict(ctx, conflicts[0].ID, synctypes.ResolutionKeepBoth), "ResolveConflict")

	// Verify it's no longer unresolved.
	remaining, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts after resolve")
	assert.Empty(t, remaining, "expected 0 unresolved conflicts")

	// Verify baseline was updated for the original file.
	bl, loadErr := eng.baseline.Load(ctx)
	require.NoError(t, loadErr)
	entry, found := bl.GetByPath("conflict-file.txt")
	require.True(t, found, "original file should have baseline entry after keep_both")
	assert.NotEqual(t, "local-h", entry.LocalHash, "baseline hash should be updated to current disk content")
}

func TestResolveConflict_NotFound(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems(nil, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	err := eng.ResolveConflict(ctx, "nonexistent-id", synctypes.ResolutionKeepBoth)
	require.Error(t, err, "expected error for nonexistent conflict")
}

func TestResolveConflict_UnknownStrategy(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems(nil, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	// Seed a conflict.
	outcomes := []synctypes.Outcome{{
		Action:       synctypes.ActionConflict,
		Success:      true,
		Path:         "bad-strategy.txt",
		DriveID:      driveID,
		ItemID:       "item-x",
		ItemType:     synctypes.ItemTypeFile,
		ConflictType: "edit_edit",
	}}

	seedBaseline(t, eng.baseline, ctx, outcomes, "")

	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts")

	err = eng.ResolveConflict(ctx, conflicts[0].ID, "invalid_strategy")
	require.Error(t, err, "expected error for unknown strategy")
}

func TestListConflicts_Engine(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems(nil, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	// Empty initially.
	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts")
	assert.Empty(t, conflicts)
}

// Validates: R-2.3.4
func TestResolveConflict_KeepLocal(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	uploadCalled := false

	mock := &engineMockClient{
		uploadFn: func(_ context.Context, _ driveid.ID, parentID, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			uploadCalled = true

			return &graph.Item{
				ID:           "uploaded-resolve",
				Name:         name,
				ETag:         "etag-resolved",
				QuickXorHash: "resolve-hash",
				Size:         5,
			}, nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	// Seed a conflict.
	outcomes := []synctypes.Outcome{{
		Action:       synctypes.ActionConflict,
		Success:      true,
		Path:         "keep-local.txt",
		DriveID:      driveID,
		ItemID:       "item-kl",
		ItemType:     synctypes.ItemTypeFile,
		LocalHash:    "local-h",
		RemoteHash:   "remote-h",
		ConflictType: "edit_edit",
	}}

	seedBaseline(t, eng.baseline, ctx, outcomes, "")

	// Write the local file that will be uploaded.
	writeLocalFile(t, syncRoot, "keep-local.txt", "local")

	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts")
	require.Len(t, conflicts, 1)

	require.NoError(t, eng.ResolveConflict(ctx, conflicts[0].ID, synctypes.ResolutionKeepLocal), "ResolveConflict")
	assert.True(t, uploadCalled, "expected Upload to be called for keep_local resolution")

	// Conflict should be resolved.
	remaining, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts after resolve")
	assert.Empty(t, remaining, "expected 0 unresolved conflicts")
}

// Validates: R-2.3.4
func TestResolveConflict_KeepRemote(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	downloadContent := "remote-version"

	mock := &engineMockClient{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, writeErr := w.Write([]byte(downloadContent))
			return int64(n), writeErr
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	// Seed a conflict.
	outcomes := []synctypes.Outcome{{
		Action:       synctypes.ActionConflict,
		Success:      true,
		Path:         "keep-remote.txt",
		DriveID:      driveID,
		ItemID:       "item-kr",
		ItemType:     synctypes.ItemTypeFile,
		LocalHash:    "local-h",
		RemoteHash:   "remote-h",
		ConflictType: "edit_edit",
	}}

	seedBaseline(t, eng.baseline, ctx, outcomes, "")

	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts")
	require.Len(t, conflicts, 1)

	require.NoError(t, eng.ResolveConflict(ctx, conflicts[0].ID, synctypes.ResolutionKeepRemote), "ResolveConflict")

	// Conflict should be resolved.
	remaining, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts after resolve")
	assert.Empty(t, remaining, "expected 0 unresolved conflicts")

	// Verify the local file has remote content.
	data := mustReadFileUnderRoot(t, syncRoot, "keep-remote.txt")
	assert.Equal(t, downloadContent, string(data))
}

func TestResolveConflict_KeepLocal_RestoreFailure(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	outcomes := []synctypes.Outcome{{
		Action:       synctypes.ActionConflict,
		Success:      true,
		Path:         "keep-local.txt",
		DriveID:      driveID,
		ItemID:       "item-kl",
		ItemType:     synctypes.ItemTypeFile,
		LocalHash:    "local-h",
		RemoteHash:   "remote-h",
		ConflictType: "edit_edit",
	}}
	seedBaseline(t, eng.baseline, ctx, outcomes, "")

	conflictCopy := syncexec.ConflictCopyPath(filepath.Join(syncRoot, "keep-local.txt"), time.Unix(1, 0))
	require.NoError(t, os.WriteFile(conflictCopy, []byte("local"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "keep-local.txt"), 0o700))

	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err)
	require.Len(t, conflicts, 1)

	err = eng.ResolveConflict(ctx, conflicts[0].ID, synctypes.ResolutionKeepLocal)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "restoring conflict copy")
}

func TestResolveConflict_KeepBoth_MissingOriginalReturnsError(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	outcomes := []synctypes.Outcome{{
		Action:       synctypes.ActionConflict,
		Success:      true,
		Path:         "missing-original.txt",
		DriveID:      driveID,
		ItemID:       "item-kb",
		ItemType:     synctypes.ItemTypeFile,
		LocalHash:    "local-h",
		RemoteHash:   "remote-h",
		ConflictType: "edit_edit",
	}}
	seedBaseline(t, eng.baseline, ctx, outcomes, "")

	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err)
	require.Len(t, conflicts, 1)

	err = eng.ResolveConflict(ctx, conflicts[0].ID, synctypes.ResolutionKeepBoth)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "updating baseline for original")
	assert.Contains(t, err.Error(), "stat missing-original.txt")
}

// ---------------------------------------------------------------------------
