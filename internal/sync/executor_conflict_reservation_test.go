package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

func conflictCopyNames(t *testing.T, syncRoot string) []string {
	t.Helper()

	entries, err := os.ReadDir(syncRoot)
	require.NoError(t, err)

	var names []string
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".conflict-") {
			names = append(names, entry.Name())
		}
	}

	return names
}

// Validates: R-6.2.4
//
// Choosing the destination by stat and then renaming onto it is a
// check-then-act. Rename silently replaces whatever sits at the destination,
// so anything appearing in the gap was destroyed by the operation whose whole
// purpose is to preserve a file. Two conflict copies resolved in the same
// second are the reachable case, and the fixed test clock reproduces it
// exactly: before the destination was claimed atomically, both calls returned
// the same path.
func TestExecutor_ConflictCopy_ReservationGivesEachCopyItsOwnDestination(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "clash.txt", "original")
	absPath := filepath.Join(syncRoot, "clash.txt")

	firstAbs, firstRel, err := e.reserveUniqueConflictCopyPath(absPath)
	require.NoError(t, err)

	secondAbs, secondRel, err := e.reserveUniqueConflictCopyPath(absPath)
	require.NoError(t, err)

	assert.NotEqual(t, firstAbs, secondAbs,
		"two conflict copies resolved in the same second must not share a destination")
	assert.NotEqual(t, firstRel, secondRel)
}

// Validates: R-6.2.4
//
// A claim that goes unused must not be left behind: the placeholder is empty,
// so it would read as a truncated conflict copy.
func TestExecutor_ConflictCopy_ReleasedReservationLeavesNoPlaceholder(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "released.txt", "original")

	_, rel, err := e.reserveUniqueConflictCopyPath(filepath.Join(syncRoot, "released.txt"))
	require.NoError(t, err)

	e.releaseConflictCopyReservation(rel)

	assert.Empty(t, conflictCopyNames(t, syncRoot))
}

// Validates: R-6.2.4
//
// The reservation is an implementation detail of choosing a name: a completed
// conflict copy still leaves exactly one file, holding the original content.
func TestExecutor_ConflictCopy_LeavesExactlyOneCopyHoldingTheOriginal(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-conflict.txt", "local edit worth keeping")

	action := &Action{
		Type:    ActionConflictCopy,
		Path:    "exec-conflict.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &pathView{
			Local:  &localState{ItemType: ItemTypeFile},
			Remote: &remoteState{ItemID: "item1", ETag: "etag1"},
		},
	}

	o := e.ExecuteConflictCopy(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	names := conflictCopyNames(t, syncRoot)
	require.Len(t, names, 1, "a conflict copy must leave exactly one file")

	contents, err := localpath.ReadFile(filepath.Join(syncRoot, names[0]))
	require.NoError(t, err)
	assert.Equal(t, "local edit worth keeping", string(contents))
}
