package syncexec

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// symlinkBoundaryFixture builds an executor whose sync root contains "alias",
// a symlink to a directory outside the root. Every local mutation targeting a
// path beneath "alias/" is lexically inside the sync tree but physically
// outside it.
type symlinkBoundaryFixture struct {
	exec     *Executor
	syncRoot string
	outside  string
}

func newSymlinkBoundaryFixture(t *testing.T) symlinkBoundaryFixture {
	t.Helper()

	return newSymlinkBoundaryFixtureWithUploader(t, &syncfixture.ExecutorMockUploader{})
}

func newSymlinkBoundaryFixtureWithUploader(t *testing.T, ul *syncfixture.ExecutorMockUploader) symlinkBoundaryFixture {
	t.Helper()

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, ul)
	exec := NewExecution(cfg, syncfixture.EmptyBaseline())

	outside := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(outside, "target"), 0o700))
	if err := os.Symlink(filepath.Join(outside, "target"), filepath.Join(syncRoot, "alias")); err != nil {
		t.Skipf("symlink not available on this filesystem: %v", err)
	}

	return symlinkBoundaryFixture{exec: exec, syncRoot: syncRoot, outside: outside}
}

// outsideContent is the sentinel written to every file outside the sync root.
// Each test asserts it is still intact afterward, so a regression shows up as
// changed bytes rather than only as a missing error.
const outsideContent = "PRECIOUS"

func (f symlinkBoundaryFixture) writeTarget(t *testing.T, name string) {
	t.Helper()

	require.NoError(t, os.WriteFile(filepath.Join(f.outside, "target", name), []byte(outsideContent), 0o600))
}

func (f symlinkBoundaryFixture) requireTargetIntact(t *testing.T, name string) {
	t.Helper()

	//nolint:gosec // test fixture reads a path it just created under t.TempDir.
	got, err := os.ReadFile(filepath.Join(f.outside, "target", name))
	require.NoError(t, err, "content outside the sync root must survive")
	assert.Equal(t, outsideContent, string(got), "content outside the sync root must be unmodified")
}

// Validates: R-2.4.6
func TestExecutor_Download_SymlinkedAncestorIsBlocked(t *testing.T) {
	t.Parallel()

	f := newSymlinkBoundaryFixture(t)
	f.writeTarget(t, "victim.txt")

	action := &syncdomain.Action{
		Type:   syncdomain.ActionDownload,
		Path:   "alias/victim.txt",
		ItemID: "item1",
		View:   &syncdomain.PathView{},
	}

	o := f.exec.ExecuteDownload(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	assert.Contains(t, o.Error.Error(), "won't download alias/victim.txt: alias is a symbolic link")
	f.requireTargetIntact(t, "victim.txt")
}

// Validates: R-2.4.6
func TestExecutor_LocalMove_SymlinkedAncestorSourceIsBlocked(t *testing.T) {
	t.Parallel()

	f := newSymlinkBoundaryFixture(t)
	f.writeTarget(t, "src.txt")

	action := &syncdomain.Action{
		Type:    syncdomain.ActionLocalMove,
		OldPath: "alias/src.txt",
		Path:    "moved.txt",
		ItemID:  "item1",
		View:    &syncdomain.PathView{},
	}

	o := f.exec.ExecuteLocalMove(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	assert.Contains(t, o.Error.Error(), "alias is a symbolic link")
	f.requireTargetIntact(t, "src.txt")
}

// Validates: R-2.4.6
func TestExecutor_LocalMove_SymlinkedAncestorDestinationIsBlocked(t *testing.T) {
	t.Parallel()

	f := newSymlinkBoundaryFixture(t)
	f.writeTarget(t, "victim.txt")
	syncfixture.WriteExecTestFile(t, f.syncRoot, "src.txt", "SOURCE")

	action := &syncdomain.Action{
		Type:    syncdomain.ActionLocalMove,
		OldPath: "src.txt",
		Path:    "alias/victim.txt",
		ItemID:  "item1",
		View:    &syncdomain.PathView{},
	}

	o := f.exec.ExecuteLocalMove(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	assert.Contains(t, o.Error.Error(), "alias is a symbolic link")
	f.requireTargetIntact(t, "victim.txt")
	assert.FileExists(t, filepath.Join(f.syncRoot, "src.txt"))
}

// Validates: R-2.4.6
func TestExecutor_ConflictCopy_SymlinkedAncestorIsBlocked(t *testing.T) {
	t.Parallel()

	f := newSymlinkBoundaryFixture(t)
	f.writeTarget(t, "doc.txt")

	action := &syncdomain.Action{
		Type:   syncdomain.ActionConflictCopy,
		Path:   "alias/doc.txt",
		ItemID: "item1",
		View:   &syncdomain.PathView{},
	}

	o := f.exec.ExecuteConflictCopy(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	assert.Contains(t, o.Error.Error(), "won't conflict-copy alias/doc.txt: alias is a symbolic link")
	f.requireTargetIntact(t, "doc.txt")
}

// Validates: R-2.4.6
func TestExecutor_CreateLocalFolder_SymlinkedAncestorIsBlocked(t *testing.T) {
	t.Parallel()

	f := newSymlinkBoundaryFixture(t)

	action := &syncdomain.Action{
		Type:       syncdomain.ActionFolderCreate,
		CreateSide: syncdomain.CreateLocal,
		Path:       "alias/newdir",
		ItemID:     "item1",
		View:       &syncdomain.PathView{},
	}

	o := f.exec.ExecuteFolderCreate(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	assert.Contains(t, o.Error.Error(), "won't create folder alias/newdir: alias is a symbolic link")
	assert.NoDirExists(t, filepath.Join(f.outside, "target", "newdir"))
}

// Validates: R-2.4.6
//
// The alias itself stays mutable: only descendants are refused.
func TestExecutor_LocalDelete_AliasItselfStillRemovesOnlyTheSymlink(t *testing.T) {
	t.Parallel()

	f := newSymlinkBoundaryFixture(t)
	f.writeTarget(t, "keep.txt")

	action := &syncdomain.Action{
		Type:   syncdomain.ActionLocalDelete,
		Path:   "alias",
		ItemID: "item1",
		View:   &syncdomain.PathView{},
	}

	o := f.exec.ExecuteLocalDelete(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)
	assert.NoFileExists(t, filepath.Join(f.syncRoot, "alias"))
	f.requireTargetIntact(t, "keep.txt")
}

// Validates: R-2.4.6
//
// Paths with no symlinked ancestor must be unaffected by the boundary check.
func TestExecutor_LocalMove_WithoutSymlinkBoundarySucceeds(t *testing.T) {
	t.Parallel()

	f := newSymlinkBoundaryFixture(t)
	syncfixture.WriteExecTestFile(t, f.syncRoot, "plain/src.txt", "SOURCE")

	action := &syncdomain.Action{
		Type:    syncdomain.ActionLocalMove,
		OldPath: "plain/src.txt",
		Path:    "plain/dst.txt",
		ItemID:  "item1",
		View:    &syncdomain.PathView{},
	}

	o := f.exec.ExecuteLocalMove(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)
	assert.FileExists(t, filepath.Join(f.syncRoot, "plain", "dst.txt"))
}

// syncAliasFolder records the alias as an already-synced folder, which is
// what makes it the remote parent of uploads beneath it.
func (f symlinkBoundaryFixture) syncAliasFolder() {
	f.exec.baseline = syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path:     "alias",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemID:   "alias-folder-id",
		ItemType: syncdomain.ItemTypeFolder,
	})
}

// plannedLocalStateFor builds the local view observation would have planned
// for a file read through the alias: the followed target's size, mtime and
// identity.
func plannedLocalStateFor(t *testing.T, absPath string, hash string) *syncdomain.LocalState {
	t.Helper()

	info, err := os.Stat(absPath)
	require.NoError(t, err)
	identity, identityOK := synctree.IdentityFromFileInfo(info)
	require.True(t, identityOK)

	return &syncdomain.LocalState{
		ItemType:         syncdomain.ItemTypeFile,
		Hash:             hash,
		Size:             info.Size(),
		Mtime:            info.ModTime().UnixNano(),
		LocalDevice:      identity.Device,
		LocalInode:       identity.Inode,
		LocalHasIdentity: true,
	}
}

// Validates: R-2.4.6
//
// Reading through a followed directory alias is what follow_symlinks is for:
// a file beneath an alias that points outside the sync root uploads with its
// target's content, and the target is left untouched.
func TestExecutor_Upload_UnderFollowedDirectoryAliasSucceeds(t *testing.T) {
	t.Parallel()

	var uploaded string
	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, _, name string, content io.ReaderAt, size int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			assert.Equal(t, "photo.txt", name)
			buf := make([]byte, size)
			_, err := content.ReadAt(buf, 0)
			require.NoError(t, err)
			uploaded = string(buf)
			return &graph.Item{ID: "uploaded-under-alias", ETag: "etag1"}, nil
		},
	}

	f := newSymlinkBoundaryFixtureWithUploader(t, ul)
	f.syncAliasFolder()
	f.writeTarget(t, "photo.txt")
	targetPath := filepath.Join(f.outside, "target", "photo.txt")
	before, err := os.Stat(targetPath)
	require.NoError(t, err)

	action := &syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "alias/photo.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &syncdomain.PathView{
			Path:  "alias/photo.txt",
			Local: plannedLocalStateFor(t, targetPath, ""),
		},
	}

	o := f.exec.ExecuteUpload(t.Context(), action)
	syncfixture.RequireOutcomeSuccess(t, &o)
	assert.Equal(t, "uploaded-under-alias", o.ItemID)
	assert.Equal(t, outsideContent, uploaded)

	f.requireTargetIntact(t, "photo.txt")
	after, err := os.Stat(targetPath)
	require.NoError(t, err)
	assert.Equal(t, before.ModTime(), after.ModTime(), "upload must not touch the target's metadata")
}

// Validates: R-2.4.6
//
// The followed source is still checked against what observation planned, so
// a target that changed after planning is a stale precondition, not an upload
// of content nobody observed.
func TestExecutor_Upload_UnderFollowedDirectoryAliasDetectsChangedTarget(t *testing.T) {
	t.Parallel()

	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			require.FailNow(t, "a changed source must not upload")
			return nil, fmt.Errorf("unexpected upload")
		},
	}

	f := newSymlinkBoundaryFixtureWithUploader(t, ul)
	f.syncAliasFolder()
	f.writeTarget(t, "photo.txt")
	targetPath := filepath.Join(f.outside, "target", "photo.txt")
	planned := plannedLocalStateFor(t, targetPath, "")
	require.NoError(t, os.WriteFile(targetPath, []byte(outsideContent+" and more"), 0o600))

	action := &syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "alias/photo.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{Path: "alias/photo.txt", Local: planned},
	}

	o := f.exec.ExecuteUpload(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
	assert.Contains(t, o.Error.Error(), "upload source alias/photo.txt changed size")
}

// Validates: R-2.4.6
//
// Allowing the read does not open a write path: the same alias path that
// just uploaded still refuses a download, and the content outside the sync
// root survives.
func TestExecutor_UnderFollowedDirectoryAlias_UploadReadsButDownloadStaysRefused(t *testing.T) {
	t.Parallel()

	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{ID: "uploaded-under-alias", ETag: "etag1"}, nil
		},
	}

	f := newSymlinkBoundaryFixtureWithUploader(t, ul)
	f.syncAliasFolder()
	f.writeTarget(t, "photo.txt")
	targetPath := filepath.Join(f.outside, "target", "photo.txt")

	upload := &syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "alias/photo.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{Path: "alias/photo.txt", Local: plannedLocalStateFor(t, targetPath, "")},
	}
	uploadOutcome := f.exec.ExecuteUpload(t.Context(), upload)
	syncfixture.RequireOutcomeSuccess(t, &uploadOutcome)

	download := &syncdomain.Action{
		Type:   syncdomain.ActionDownload,
		Path:   "alias/photo.txt",
		ItemID: "uploaded-under-alias",
		View:   &syncdomain.PathView{},
	}
	downloadOutcome := f.exec.ExecuteDownload(t.Context(), download)
	syncfixture.RequireOutcomeFailure(t, &downloadOutcome)
	assert.Contains(t, downloadOutcome.Error.Error(), "won't download alias/photo.txt: alias is a symbolic link")
	f.requireTargetIntact(t, "photo.txt")
}

// Validates: R-2.4.6
//
// Following an alias for a read never widens what an upload path may name:
// a path that lexically leaves the sync root is refused before any stat.
func TestExecutor_Upload_PathLeavingSyncRootIsRefused(t *testing.T) {
	t.Parallel()

	f := newSymlinkBoundaryFixture(t)
	f.writeTarget(t, "photo.txt")

	action := &syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "../photo.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{Path: "../photo.txt"},
	}

	o := f.exec.ExecuteUpload(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrPathEscapesSyncRoot)
	require.ErrorIs(t, o.Error, synctree.ErrUnsafePath)
}
