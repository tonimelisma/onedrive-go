package sync

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// caseVariantRenameKeepsTheOldSpelling answers a rename onto a case variant
// of its own name as done and leaves the old spelling, as a case-insensitive
// volume that takes a spelling-only rename as a no-op does, so the rename
// goes through a temporary sibling.
type caseVariantRenameKeepsTheOldSpelling struct {
	synctree.Handle
}

func (h caseVariantRenameKeepsTheOldSpelling) Rename(oldname, newname string) error {
	if oldname != newname && strings.EqualFold(oldname, newname) {
		return nil
	}

	return h.Handle.Rename(oldname, newname) //nolint:wrapcheck // passes the wrapped handle's answer through
}

// listingHidesName is caseVariantRenameKeepsTheOldSpelling whose folder
// listings leave out every entry called hidden. With hidden a hard link of
// the entry under another spelling, the sync tree reads as a volume that
// folds case: a lstat of either spelling finds the entry, a no-replace
// rename onto the other spelling finds it taken, a plain one changes
// nothing, and a listing holds the old spelling only.
type listingHidesName struct {
	caseVariantRenameKeepsTheOldSpelling
	hidden string
}

func (h listingHidesName) FS() fs.FS {
	return listingWithoutName{FS: h.Handle.FS(), hidden: h.hidden}
}

// listingWithoutName is a filesystem view whose folder listings leave out
// every entry called hidden.
type listingWithoutName struct {
	fs.FS
	hidden string
}

func (f listingWithoutName) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(f.FS, name)

	return slices.DeleteFunc(entries, func(entry fs.DirEntry) bool { return entry.Name() == f.hidden }), err
}

// Validates: R-2.12.4, R-6.4.8
//
// A spelling-only move on a volume that folds case renames through a
// temporary sibling, and when its second step finds the new spelling taken
// the entry is put back and the move is a plain failure of the rename, not a
// destination taken before the move: the precondition that a replan would
// check again did not change, the rename failed half way.
func TestExecutor_LocalMove_SpellingRenameWhoseSecondStepFindsTheNameTakenFails(t *testing.T) {
	t.Parallel()
	skipUnlessNamesCoexist(t, "Notes.txt", "notes.txt")

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, "Notes.txt", "content")
	require.NoError(t, os.Link(filepath.Join(syncRoot, "Notes.txt"), filepath.Join(syncRoot, "notes.txt")))
	e.syncTree = e.syncTree.Interpose(func(h synctree.Handle) synctree.Handle {
		return listingHidesName{caseVariantRenameKeepsTheOldSpelling: caseVariantRenameKeepsTheOldSpelling{Handle: h}, hidden: "notes.txt"}
	})

	o := e.ExecuteMove(t.Context(), &Action{
		Type: ActionLocalMove, Path: "notes.txt", OldPath: "Notes.txt", View: &PathView{Path: "notes.txt"},
	})

	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, synctree.ErrTargetExists)
	require.NotErrorIs(t, o.Error, ErrActionPreconditionChanged)
	require.ErrorContains(t, o.Error, "renaming Notes.txt -> notes.txt")
	requireExecFileContent(t, syncRoot, "Notes.txt", "content")
	assert.ElementsMatch(t, []string{"Notes.txt", "notes.txt"}, localNames(t, syncRoot), "the entry is back under its old spelling")
}

// Validates: R-2.12.4, R-6.4.8
//
// A spelling-only rename onto a new spelling that names another file, on a
// volume that tells the spellings apart, replaces nothing: the no-replace
// rename finds the name taken, the two names are found to hold different
// files, so no plain rename follows, both files keep their content, and the
// move ends as a destination taken before it, which replans it.
func TestExecutor_RenameLocal_SpellingRenameOntoAnotherFileKeepsBoth(t *testing.T) {
	t.Parallel()
	skipUnlessNamesCoexist(t, "Plan.txt", "plan.txt")

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, "Plan.txt", "moving\n")
	writeExecTestFile(t, syncRoot, "plan.txt", "another file\n")

	err := e.renameLocal("Plan.txt", "plan.txt", true)

	require.ErrorIs(t, err, ErrActionPreconditionChanged)
	require.ErrorIs(t, err, synctree.ErrTargetExists)
	requireExecFileContent(t, syncRoot, "Plan.txt", "moving\n")
	requireExecFileContent(t, syncRoot, "plan.txt", "another file\n")
	assert.ElementsMatch(t, []string{"Plan.txt", "plan.txt"}, localNames(t, syncRoot))
}

// Validates: R-2.12.4, R-6.4.8
//
// A spelling-only rename that left the old spelling in its folder, as a
// volume that takes it as a no-op does, goes on through a temporary sibling;
// when the sibling reached the new spelling but could not be removed, the
// rename happened: respellLocal returns no error, the entry is at its new
// spelling, and a warning says a name it was renamed from is left.
func TestExecutor_RenameLocal_SpellingRenameThatLeavesItsTemporaryNameIsDone(t *testing.T) {
	t.Parallel()
	synctest.SkipWithoutHardLinks(t)

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())
	capture, logger := synctest.NewLogCapture()
	e.logger = logger
	temporary := SpellingRenameArtifactPrefix + "docs.txt"
	e.syncTree = e.syncTree.WithoutNoReplaceFlagAndUnremovableName(temporary)
	writeExecTestFile(t, syncRoot, "Docs.txt", "content")

	require.NoError(t, e.respellLocal("docs.txt"))
	requireExecFileContent(t, syncRoot, "docs.txt", "content")
	requireExecFileContent(t, syncRoot, temporary, "content")
	assert.ElementsMatch(t, []string{"docs.txt", temporary}, localNames(t, syncRoot))
	warnings := capture.AtLeast(slog.LevelWarn)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0].Message, "could not remove a name it was renamed from")
}

// Validates: R-2.12.3, R-2.12.4
//
// The spelling a folder holds for a name is the entry spelled so, else the
// only entry OneDrive treats as that name; two such entries and none spelled
// so are an error naming the folder, and neither is renamed onto the name.
func TestExecutor_StoredNameSpelling_TwoKeyEqualEntriesAreAnError(t *testing.T) {
	t.Parallel()
	skipUnlessNamesCoexist(t, "Docs.txt", "DOCS.txt")

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, "Docs.txt", "first")

	stored, err := e.storedNameSpelling("docs.txt")
	require.NoError(t, err)
	assert.Equal(t, "Docs.txt", stored, "the one entry of the name")

	writeExecTestFile(t, syncRoot, "DOCS.txt", "second")
	_, err = e.storedNameSpelling("docs.txt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "holds more than one entry OneDrive treats as that name")
	require.NotErrorIs(t, err, ErrActionPreconditionChanged, "a collision the user sees, not a replan")

	require.Error(t, e.respellLocal("docs.txt"))
	requireExecFileContent(t, syncRoot, "Docs.txt", "first")
	requireExecFileContent(t, syncRoot, "DOCS.txt", "second")
	assert.ElementsMatch(t, []string{"Docs.txt", "DOCS.txt"}, localNames(t, syncRoot))

	writeExecTestFile(t, syncRoot, "docs.txt", "exact")
	stored, err = e.storedNameSpelling("docs.txt")
	require.NoError(t, err)
	assert.Equal(t, "docs.txt", stored, "the entry spelled so wins over the others")
}

// Validates: R-2.13.2
//
// A folder holding two entries of a path's stored form (its NFC and NFD
// spellings, which a byte-exact filesystem keeps apart) holds the name: the
// answer stays "exists", and one entry alone is found by either spelling.
func TestExecutor_ExactLocalNameExists_TwoEntriesOfOneStoredForm(t *testing.T) {
	t.Parallel()
	const (
		nfc = "caf\u00e9.txt"  // LATIN SMALL LETTER E WITH ACUTE
		nfd = "cafe\u0301.txt" // e and COMBINING ACUTE ACCENT
	)
	skipUnlessNamesCoexist(t, nfc, nfd)

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	assert.False(t, e.exactLocalNameExists(nfc), "no entry")
	writeExecTestFile(t, syncRoot, nfd, "decomposed")
	assert.True(t, e.exactLocalNameExists(nfc), "the NFD entry holds the NFC path")
	assert.False(t, e.exactLocalNameExists("CAF\u00c9.txt"), "another stored form is another name here")
	writeExecTestFile(t, syncRoot, nfc, "composed")
	assert.True(t, e.exactLocalNameExists(nfc), "two entries of the stored form")
}
