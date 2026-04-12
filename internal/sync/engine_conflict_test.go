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
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

const engineResolvedRemoteVersion = "remote-version"

// ---------------------------------------------------------------------------
// Conflict resolution tests
// ---------------------------------------------------------------------------

func TestResolveConflict_KeepBoth(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	eng, syncRoot := newTestEngine(t, &engineMockClient{})
	ctx := t.Context()

	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "conflict-file.txt"), []byte("local content"), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(syncRoot, "conflict-file.conflict-20260115-120000-2.txt"),
		[]byte("other local content"),
		0o600,
	))

	outcomes := []ExecutionResult{{
		Action:       ActionConflict,
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

	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts")
	require.Len(t, conflicts, 1)

	require.NoError(t, eng.ResolveConflict(ctx, conflicts[0].ID, synctypes.ResolutionKeepBoth), "ResolveConflict")

	remaining, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts after resolve")
	assert.Empty(t, remaining, "expected 0 unresolved conflicts")

	assert.Equal(t, "local content", string(mustReadFileUnderRoot(t, syncRoot, "conflict-file.txt")))
	assert.Equal(
		t,
		"other local content",
		string(mustReadFileUnderRoot(t, syncRoot, "conflict-file.conflict-20260115-120000-2.txt")),
	)
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
	outcomes := []ExecutionResult{{
		Action:       ActionConflict,
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
		uploadToItemFn: func(_ context.Context, _ driveid.ID, itemID string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			uploadCalled = true
			assert.Equal(t, "item-kl", itemID)

			return &graph.Item{
				ID:           "uploaded-resolve",
				Name:         "keep-local.txt",
				ETag:         "etag-resolved",
				QuickXorHash: "resolve-hash",
				Size:         5,
			}, nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	// Seed a conflict.
	outcomes := []ExecutionResult{{
		Action:       ActionConflict,
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

	writeLocalFile(t, syncRoot, "keep-local.txt", "remote content")
	writeLocalFile(t, syncRoot, "keep-local.conflict-20260115-120000.txt", "local preferred")

	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts")
	require.Len(t, conflicts, 1)

	require.NoError(t, eng.ResolveConflict(ctx, conflicts[0].ID, synctypes.ResolutionKeepLocal), "ResolveConflict")
	assert.False(t, uploadCalled, "keep_local should only restore layout; upload is ordinary sync work")

	remaining, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts after resolve")
	assert.Empty(t, remaining, "expected 0 unresolved conflicts")
	assert.Equal(t, "local preferred", string(mustReadFileUnderRoot(t, syncRoot, "keep-local.txt")))
}

// Validates: R-2.3.4
func TestResolveConflict_KeepLocal_RestoresSuffixedConflictCopy(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	uploadCalled := false

	mock := &engineMockClient{
		uploadToItemFn: func(_ context.Context, _ driveid.ID, itemID string, content io.ReaderAt, size int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			assert.Equal(t, "item-kl", itemID)
			uploadCalled = true

			return &graph.Item{
				ID:           "uploaded-suffixed",
				Name:         "keep-local.txt",
				ETag:         "etag-suffixed",
				QuickXorHash: "resolve-hash",
				Size:         size,
			}, nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	outcomes := []ExecutionResult{{
		Action:       ActionConflict,
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

	writeLocalFile(t, syncRoot, "keep-local.txt", "remote content")
	writeLocalFile(t, syncRoot, "keep-local.conflict-20260115-120000-2.txt", "local preferred")

	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err)
	require.Len(t, conflicts, 1)

	require.NoError(t, eng.ResolveConflict(ctx, conflicts[0].ID, synctypes.ResolutionKeepLocal))
	assert.False(t, uploadCalled, "keep-local should only restore the chosen local layout")
	assert.Equal(t, "local preferred", string(mustReadFileUnderRoot(t, syncRoot, "keep-local.txt")))

	_, statErr := os.Stat(filepath.Join(syncRoot, "keep-local.conflict-20260115-120000-2.txt"))
	assert.True(t, os.IsNotExist(statErr), "resolved keep-local should remove suffixed conflict copies")
}

// Validates: R-2.3.4
func TestResolveConflict_KeepRemote(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	downloadCalled := false

	mock := &engineMockClient{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			downloadCalled = true
			n, writeErr := w.Write([]byte("remote-version"))
			return int64(n), writeErr
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	// Seed a conflict.
	outcomes := []ExecutionResult{{
		Action:       ActionConflict,
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
	writeLocalFile(t, syncRoot, "keep-remote.txt", engineResolvedRemoteVersion)
	writeLocalFile(t, syncRoot, "keep-remote.conflict-20260115-120000.txt", "local version")

	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts")
	require.Len(t, conflicts, 1)

	require.NoError(t, eng.ResolveConflict(ctx, conflicts[0].ID, synctypes.ResolutionKeepRemote), "ResolveConflict")
	assert.False(t, downloadCalled, "keep_remote should not trigger a special re-download")

	remaining, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts after resolve")
	assert.Empty(t, remaining, "expected 0 unresolved conflicts")

	data := mustReadFileUnderRoot(t, syncRoot, "keep-remote.txt")
	assert.Equal(t, engineResolvedRemoteVersion, string(data))
}

// Validates: R-2.3.4
func TestResolveConflict_KeepRemote_CleansUpSuffixedConflictCopy(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	downloadCalled := false

	mock := &engineMockClient{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			downloadCalled = true
			n, writeErr := w.Write([]byte(engineResolvedRemoteVersion))
			return int64(n), writeErr
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	outcomes := []ExecutionResult{{
		Action:       ActionConflict,
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

	writeLocalFile(t, syncRoot, "keep-remote.txt", engineResolvedRemoteVersion)
	writeLocalFile(t, syncRoot, "keep-remote.conflict-20260115-120000-2.txt", "local copy")

	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err)
	require.Len(t, conflicts, 1)

	require.NoError(t, eng.ResolveConflict(ctx, conflicts[0].ID, synctypes.ResolutionKeepRemote))
	assert.False(t, downloadCalled, "keep_remote should not perform a special re-download")

	_, statErr := os.Stat(filepath.Join(syncRoot, "keep-remote.conflict-20260115-120000-2.txt"))
	assert.True(t, os.IsNotExist(statErr), "keep-remote cleanup should remove suffixed conflict copies")
	assert.Equal(t, engineResolvedRemoteVersion, string(mustReadFileUnderRoot(t, syncRoot, "keep-remote.txt")))
}

func TestResolveConflict_KeepLocal_RestoreFailure(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	outcomes := []ExecutionResult{{
		Action:       ActionConflict,
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

	conflictCopy := ConflictCopyPath(filepath.Join(syncRoot, "keep-local.txt"), time.Unix(1, 0))
	require.NoError(t, os.WriteFile(conflictCopy, []byte("local"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "keep-local.txt"), 0o700))

	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err)
	require.Len(t, conflicts, 1)

	err = eng.ResolveConflict(ctx, conflicts[0].ID, synctypes.ResolutionKeepLocal)
	require.NoError(t, err)

	failed, err := eng.baseline.GetConflictRequest(ctx, conflicts[0].ID)
	require.NoError(t, err)
	assert.Equal(t, synctypes.ConflictStateQueued, failed.State)
	assert.Contains(t, failed.LastError, "restoring conflict copy")

	remaining, err := eng.ListConflicts(ctx)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
}

func TestResolveConflict_KeepBoth_MissingOriginalReturnsError(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	outcomes := []ExecutionResult{{
		Action:       ActionConflict,
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
	require.NoError(t, err)

	failed, err := eng.baseline.GetConflictRequest(ctx, conflicts[0].ID)
	require.NoError(t, err)
	assert.Equal(t, synctypes.ConflictStateQueued, failed.State)
	assert.Contains(t, failed.LastError, "confirming original file for keep-both")
	assert.Contains(t, failed.LastError, "missing-original.txt")

	remaining, err := eng.ListConflicts(ctx)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
}

// Validates: R-2.3.4
func TestResolveConflict_KeepBoth_KeepsSuffixedConflictCopy(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "conflict-file.txt"), []byte("local content"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "conflict-file.conflict-20260115-120000-2.txt"), []byte("other local content"), 0o600))

	outcomes := []ExecutionResult{{
		Action:       ActionConflict,
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

	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err)
	require.Len(t, conflicts, 1)

	require.NoError(t, eng.ResolveConflict(ctx, conflicts[0].ID, synctypes.ResolutionKeepBoth))

	remaining, err := eng.ListConflicts(ctx)
	require.NoError(t, err)
	assert.Empty(t, remaining)
	assert.Equal(t, "other local content",
		string(mustReadFileUnderRoot(t, syncRoot, "conflict-file.conflict-20260115-120000-2.txt")))
}

// ---------------------------------------------------------------------------
