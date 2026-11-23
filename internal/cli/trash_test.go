package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
)

// seedTrashEntry puts one file into the trash of the drive whose state lives
// at statePath, as sync would after deleting syncRoot/rel, and returns its ID.
func seedTrashEntry(t *testing.T, statePath string, syncRoot string, rel string) string {
	t.Helper()

	return seedTrashEntryIn(t, localtrash.DirForStatePath(statePath), syncRoot, rel, localtrash.ReasonDeleted)
}

// seedTrashEntryIn puts one file, trashed for reason, into the trash at
// trashDir, as sync would after taking syncRoot/rel out of the sync folder,
// and returns its ID.
func seedTrashEntryIn(t *testing.T, trashDir, syncRoot, rel string, reason localtrash.Reason) string {
	t.Helper()

	trash := localtrash.Open(trashDir, nil)
	placement, err := trash.Reserve(syncRoot, rel, reason)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(placement.ItemPath, []byte("kept\n"), 0o600))
	entry, err := placement.Commit()
	require.NoError(t, err)

	return entry.ID
}

func newTrashTestContext(t *testing.T) (*CLIContext, *bytes.Buffer, driveid.CanonicalID) {
	t.Helper()
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:user@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, t.TempDir()))

	var out bytes.Buffer
	return &CLIContext{
		Flags:        CLIFlags{Drive: []string{cid.String()}},
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		StatusWriter: &out,
		CfgPath:      cfgPath,
	}, &out, cid
}

// Validates: R-6.4.9
func TestTrashCommands_ListRestoreAndEmpty(t *testing.T) {
	cc, out, cid := newTrashTestContext(t)
	syncRoot := t.TempDir()
	id := seedTrashEntry(t, config.DriveStatePath(cid), syncRoot, "Docs/report.txt")
	seedTrashEntry(t, config.DriveStatePath(cid), syncRoot, "other.txt")

	require.NoError(t, runTrashList(t.Context(), cc))
	assert.Contains(t, out.String(), "deleted Docs/report.txt")
	assert.Contains(t, out.String(), id)

	out.Reset()
	require.NoError(t, runTrashRestore(t.Context(), cc, id))
	data, err := os.ReadFile(filepath.Join(syncRoot, "Docs", "report.txt")) //nolint:gosec // test reads its own temp tree
	require.NoError(t, err)
	assert.Equal(t, "kept\n", string(data))

	err = runTrashRestore(t.Context(), cc, id)
	require.ErrorIs(t, err, localtrash.ErrEntryNotFound, "a restored entry leaves the trash")

	require.Error(t, runTrashEmpty(t.Context(), cc, false), "emptying needs --confirm")
	out.Reset()
	require.NoError(t, runTrashEmpty(t.Context(), cc, true))
	assert.Equal(t, "Permanently deleted 1 file from the local trash.\n", out.String())
}

// Validates: R-6.4.9
func TestTrashRestore_NeverOverwrites(t *testing.T) {
	cc, _, cid := newTrashTestContext(t)
	syncRoot := t.TempDir()
	id := seedTrashEntry(t, config.DriveStatePath(cid), syncRoot, "a.txt")
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "a.txt"), []byte("current\n"), 0o600))

	err := runTrashRestore(t.Context(), cc, id)
	require.ErrorIs(t, err, localtrash.ErrRestoreTargetExists)
	data, readErr := os.ReadFile(filepath.Join(syncRoot, "a.txt")) //nolint:gosec // test reads its own temp tree
	require.NoError(t, readErr)
	assert.Equal(t, "current\n", string(data))
}

// restoreFailedAfterPlacing is the error of a restore that put the file back
// and then could not make it durable.
func restoreFailedAfterPlacing() error {
	return fmt.Errorf("%w: syncing the folder: input/output error; the trash keeps its copy as entry e1",
		localtrash.ErrRestoreNotDurable)
}

// Validates: R-6.4.9
func TestReportTrashRestore_PlacedFileWithFailureIsPrintedAsRestored(t *testing.T) {
	cc, out, cid := newTrashTestContext(t)

	err := reportTrashRestore(cc, cid.String(), "e1", "/sync/Docs/report.txt", restoreFailedAfterPlacing())

	require.ErrorIs(t, err, localtrash.ErrRestoreNotDurable, "the command fails although the file is back")
	assert.Contains(t, out.String(), "Restored /sync/Docs/report.txt.", "the user is told where the file is")
}

// Validates: R-6.4.9
func TestReportTrashRestore_JSONCarriesTheWarning(t *testing.T) {
	cc, out, cid := newTrashTestContext(t)
	cc.Flags.JSON = true
	failure := restoreFailedAfterPlacing()

	err := reportTrashRestore(cc, cid.String(), "e1", "/sync/Docs/report.txt", failure)

	require.ErrorIs(t, err, localtrash.ErrRestoreNotDurable)
	var doc trashRestoreJSONOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &doc))
	assert.Equal(t, "/sync/Docs/report.txt", doc.RestoredTo)
	assert.Equal(t, failure.Error(), doc.Warning)
}

// Validates: R-6.4.9
func TestReportTrashRestore_CleanRestoreHasNoWarning(t *testing.T) {
	cc, out, cid := newTrashTestContext(t)
	cc.Flags.JSON = true

	require.NoError(t, reportTrashRestore(cc, cid.String(), "e1", "/sync/a.txt", nil))

	assert.NotContains(t, out.String(), "warning")
}

// Validates: R-6.4.9
func TestReportTrashRestore_NothingPlacedPrintsNothing(t *testing.T) {
	cc, out, cid := newTrashTestContext(t)
	cc.Flags.JSON = true

	err := reportTrashRestore(cc, cid.String(), "e1", "", localtrash.ErrRestoreTargetExists)

	require.ErrorIs(t, err, localtrash.ErrRestoreTargetExists)
	assert.Empty(t, out.String(), "a restore that placed nothing prints no document")
	assert.NotErrorIs(t, err, localtrash.ErrRestoreNotDurable)
}

// childTrashDir is the trash of the shortcut child binding of the configured
// drive cid: the child trash its engine writes and its purge hands over.
func childTrashDir(cid driveid.CanonicalID, binding string) string {
	return localtrash.ChildDir(localtrash.DirForStatePath(config.DriveStatePath(cid)),
		config.MountStatePath(config.ChildMountID(cid.String(), binding)))
}

// A shortcut child is a mount, not a configured drive: its trash is reached
// through its parent drive's trash commands, which list, restore and empty
// the parent's own entries and the child's alike.
//
// Validates: R-6.4.9, R-6.4.12
func TestTrashCommands_TheDrivesTrashCoversItsShortcutChildren(t *testing.T) {
	cc, out, cid := newTrashTestContext(t)
	syncRoot := t.TempDir()
	childRoot := t.TempDir()
	ownID := seedTrashEntry(t, config.DriveStatePath(cid), syncRoot, "own.txt")
	childID := seedTrashEntryIn(t, childTrashDir(cid, "binding-docs"), childRoot, "Shared/report.txt", localtrash.ReasonDeleted)
	seedTrashEntryIn(t, childTrashDir(cid, "binding-docs"), childRoot, "Shared/other.txt", localtrash.ReasonDeleted)

	require.NoError(t, runTrashList(t.Context(), cc))
	assert.Contains(t, out.String(), ownID)
	assert.Contains(t, out.String(), childID)
	assert.Contains(t, out.String(), "deleted Shared/report.txt")

	out.Reset()
	cc.Flags.JSON = true
	require.NoError(t, runTrashList(t.Context(), cc))
	var listed trashListJSONOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &listed))
	assert.Equal(t, cid.String(), listed.Drive)
	syncRoots := make(map[string]string, len(listed.Entries))
	for _, entry := range listed.Entries {
		syncRoots[entry.ID] = entry.SyncRoot
	}
	assert.Len(t, syncRoots, 3)
	assert.Equal(t, childRoot, syncRoots[childID], "each entry names the sync folder it came from")
	assert.Equal(t, syncRoot, syncRoots[ownID])
	cc.Flags.JSON = false

	out.Reset()
	require.NoError(t, runTrashRestore(t.Context(), cc, childID))
	data, err := os.ReadFile(filepath.Join(childRoot, "Shared", "report.txt")) //nolint:gosec // test reads its own temp tree
	require.NoError(t, err)
	assert.Equal(t, "kept\n", string(data))

	out.Reset()
	require.NoError(t, runTrashEmpty(t.Context(), cc, true))
	assert.Equal(t, "Permanently deleted 2 files from the local trash.\n", out.String(), "both trashes are emptied")
	out.Reset()
	require.NoError(t, runTrashList(t.Context(), cc))
	assert.Equal(t, "The local trash is empty.\n", out.String())
}

// A file the release of a removed shared folder put in the trash says so:
// it was not deleted on OneDrive.
//
// Validates: R-6.4.13
func TestTrashList_ReleasedEntrySaysItsSharedFolderWasRemoved(t *testing.T) {
	cc, out, cid := newTrashTestContext(t)
	childRoot := t.TempDir()
	id := seedTrashEntryIn(t, childTrashDir(cid, "binding-docs"), childRoot, "report.txt", localtrash.ReasonReleased)

	require.NoError(t, runTrashList(t.Context(), cc))
	assert.Contains(t, out.String(), id+"  ")
	assert.Contains(t, out.String(), "released report.txt ("+formatSize(int64(len("kept\n")))+
		"): its shared folder was removed from this account")
	assert.NotContains(t, out.String(), "deleted")

	out.Reset()
	cc.Flags.JSON = true
	require.NoError(t, runTrashList(t.Context(), cc))
	var listed trashListJSONOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &listed))
	require.Len(t, listed.Entries, 1)
	assert.Equal(t, "released", listed.Entries[0].Reason)
}

// A restore of a released file whose shared folder's sync folder is gone
// does not create that folder again: it says so and names where the file is
// in the trash, which keeps it.
//
// Validates: R-6.4.9, R-6.4.13
func TestTrashRestore_IntoASyncFolderThatIsGoneNamesWhereTheFileIs(t *testing.T) {
	cc, out, cid := newTrashTestContext(t)
	childRoot := filepath.Join(t.TempDir(), "Shared")
	trashDir := childTrashDir(cid, "binding-docs")
	id := seedTrashEntryIn(t, trashDir, childRoot, "report.txt", localtrash.ReasonReleased)

	err := runTrashRestore(t.Context(), cc, id)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "its sync folder "+childRoot+" does not exist")
	assert.Contains(t, err.Error(), filepath.Join(trashDir, id, "item"), "the user is told where the file is")
	assert.NoDirExists(t, childRoot, "a removed shared folder's sync folder is not created again")
	assert.Empty(t, out.String())
	entries, listErr := localtrash.Open(trashDir, nil).List()
	require.NoError(t, listErr)
	require.Len(t, entries, 1, "the file stays in the trash")
}

// trashReasonCases are the reasons trashReasonText's switch names, read from
// its source.
func trashReasonCases(t *testing.T) []string {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), "trash.go", nil, 0)
	require.NoError(t, err)
	var cases []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "trashReasonText" {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			clause, ok := node.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range clause.List {
				if selector, ok := expr.(*ast.SelectorExpr); ok {
					cases = append(cases, selector.Sel.Name)
				}
			}
			return true
		})
	}
	sort.Strings(cases)
	return cases
}

// declaredTrashReasons are the Reason constants localtrash declares, read
// from its source.
func declaredTrashReasons(t *testing.T) []string {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "localtrash", "localtrash.go"), nil, 0)
	require.NoError(t, err)
	var reasons []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if typeName, ok := value.Type.(*ast.Ident); ok && typeName.Name == "Reason" {
				for _, name := range value.Names {
					reasons = append(reasons, name.Name)
				}
			}
		}
	}
	sort.Strings(reasons)
	return reasons
}

// How `trash list` words a reason is one switch over every reason the trash
// declares, so a new reason cannot be listed as another's; a reason outside
// the set, which a later version may have written, is listed with its stored
// word.
//
// Validates: R-6.4.13
func TestTrashReasonText_NamesEveryDeclaredReason(t *testing.T) {
	t.Parallel()

	declared := declaredTrashReasons(t)
	require.NotEmpty(t, declared)
	assert.Equal(t, declared, trashReasonCases(t), "the switch names each declared reason once")

	words := make(map[string]struct{})
	for _, reason := range []localtrash.Reason{localtrash.ReasonDeleted, localtrash.ReasonReplaced, localtrash.ReasonReleased} {
		what, _ := trashReasonText(reason)
		words[what] = struct{}{}
	}
	assert.Len(t, words, len(declared), "no two reasons read alike")

	what, why := trashReasonText(localtrash.Reason("archived"))
	assert.Equal(t, "archived", what)
	assert.Empty(t, why)
}
