package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// symlinkBoundaryFixture builds an executor whose sync root contains "alias",
// a symlink to a directory outside the root. Every local mutation targeting a
// path beneath "alias/" is lexically inside the sync tree but physically
// outside it.
type symlinkBoundaryFixture struct {
	exec     *executor
	syncRoot string
	outside  string
}

func newSymlinkBoundaryFixture(t *testing.T) symlinkBoundaryFixture {
	t.Helper()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	exec := newExecution(cfg, emptyBaseline())

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

	action := &Action{
		Type:   ActionDownload,
		Path:   "alias/victim.txt",
		ItemID: "item1",
		View:   &pathView{},
	}

	o := f.exec.ExecuteDownload(t.Context(), action)
	requireOutcomeFailure(t, &o)
	assert.Contains(t, o.Error.Error(), "refusing to download alias/victim.txt through symlink boundary alias")
	f.requireTargetIntact(t, "victim.txt")
}

// Validates: R-2.4.6
func TestExecutor_LocalMove_SymlinkedAncestorSourceIsBlocked(t *testing.T) {
	t.Parallel()

	f := newSymlinkBoundaryFixture(t)
	f.writeTarget(t, "src.txt")

	action := &Action{
		Type:    ActionLocalMove,
		OldPath: "alias/src.txt",
		Path:    "moved.txt",
		ItemID:  "item1",
		View:    &pathView{},
	}

	o := f.exec.ExecuteLocalMove(action)
	requireOutcomeFailure(t, &o)
	assert.Contains(t, o.Error.Error(), "symlink boundary alias")
	f.requireTargetIntact(t, "src.txt")
}

// Validates: R-2.4.6
func TestExecutor_LocalMove_SymlinkedAncestorDestinationIsBlocked(t *testing.T) {
	t.Parallel()

	f := newSymlinkBoundaryFixture(t)
	f.writeTarget(t, "victim.txt")
	writeExecTestFile(t, f.syncRoot, "src.txt", "SOURCE")

	action := &Action{
		Type:    ActionLocalMove,
		OldPath: "src.txt",
		Path:    "alias/victim.txt",
		ItemID:  "item1",
		View:    &pathView{},
	}

	o := f.exec.ExecuteLocalMove(action)
	requireOutcomeFailure(t, &o)
	assert.Contains(t, o.Error.Error(), "symlink boundary alias")
	f.requireTargetIntact(t, "victim.txt")
	assert.FileExists(t, filepath.Join(f.syncRoot, "src.txt"))
}

// Validates: R-2.4.6
func TestExecutor_ConflictCopy_SymlinkedAncestorIsBlocked(t *testing.T) {
	t.Parallel()

	f := newSymlinkBoundaryFixture(t)
	f.writeTarget(t, "doc.txt")

	action := &Action{
		Type:   ActionConflictCopy,
		Path:   "alias/doc.txt",
		ItemID: "item1",
		View:   &pathView{},
	}

	o := f.exec.ExecuteConflictCopy(t.Context(), action)
	requireOutcomeFailure(t, &o)
	assert.Contains(t, o.Error.Error(), "refusing to conflict-copy alias/doc.txt through symlink boundary alias")
	f.requireTargetIntact(t, "doc.txt")
}

// Validates: R-2.4.6
func TestExecutor_CreateLocalFolder_SymlinkedAncestorIsBlocked(t *testing.T) {
	t.Parallel()

	f := newSymlinkBoundaryFixture(t)

	action := &Action{
		Type:       ActionFolderCreate,
		CreateSide: createLocal,
		Path:       "alias/newdir",
		ItemID:     "item1",
		View:       &pathView{},
	}

	o := f.exec.ExecuteFolderCreate(t.Context(), action)
	requireOutcomeFailure(t, &o)
	assert.Contains(t, o.Error.Error(), "refusing to create folder alias/newdir through symlink boundary alias")
	assert.NoDirExists(t, filepath.Join(f.outside, "target", "newdir"))
}

// Validates: R-2.4.6
//
// The alias itself stays mutable: only descendants are refused.
func TestExecutor_LocalDelete_AliasItselfStillRemovesOnlyTheSymlink(t *testing.T) {
	t.Parallel()

	f := newSymlinkBoundaryFixture(t)
	f.writeTarget(t, "keep.txt")

	action := &Action{
		Type:   ActionLocalDelete,
		Path:   "alias",
		ItemID: "item1",
		View:   &pathView{},
	}

	o := f.exec.ExecuteLocalDelete(t.Context(), action)
	requireOutcomeSuccess(t, &o)
	assert.NoFileExists(t, filepath.Join(f.syncRoot, "alias"))
	f.requireTargetIntact(t, "keep.txt")
}

// Validates: R-2.4.6
//
// Paths with no symlinked ancestor must be unaffected by the boundary check.
func TestExecutor_LocalMove_WithoutSymlinkBoundarySucceeds(t *testing.T) {
	t.Parallel()

	f := newSymlinkBoundaryFixture(t)
	writeExecTestFile(t, f.syncRoot, "plain/src.txt", "SOURCE")

	action := &Action{
		Type:    ActionLocalMove,
		OldPath: "plain/src.txt",
		Path:    "plain/dst.txt",
		ItemID:  "item1",
		View:    &pathView{},
	}

	o := f.exec.ExecuteLocalMove(action)
	requireOutcomeSuccess(t, &o)
	assert.FileExists(t, filepath.Join(f.syncRoot, "plain", "dst.txt"))
}
