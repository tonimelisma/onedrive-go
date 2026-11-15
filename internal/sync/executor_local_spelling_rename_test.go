package sync

import (
	"log/slog"
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

// Validates: R-2.12.4, R-6.4.8
//
// A spelling-only rename whose temporary sibling reached the new spelling but
// could not be removed is a rename that happened: renameLocal returns no
// error, the entry is at its new spelling, and a warning says a name it was
// renamed from is left.
func TestExecutor_RenameLocal_SpellingRenameThatLeavesItsTemporaryNameIsDone(t *testing.T) {
	t.Parallel()
	synctest.SkipWithoutHardLinks(t)

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())
	capture, logger := synctest.NewLogCapture()
	e.logger = logger
	temporary := spellingRenameArtifactPrefix + "docs.txt"
	e.syncTree = e.syncTree.
		Interpose(func(h synctree.Handle) synctree.Handle { return caseVariantRenameKeepsTheOldSpelling{Handle: h} }).
		WithoutNoReplaceFlagAndUnremovableName(temporary)
	writeExecTestFile(t, syncRoot, "Docs.txt", "content")

	require.NoError(t, e.renameLocal("Docs.txt", "docs.txt", true))
	requireExecFileContent(t, syncRoot, "docs.txt", "content")
	requireExecFileContent(t, syncRoot, temporary, "content")
	assert.ElementsMatch(t, []string{"docs.txt", temporary}, localNames(t, syncRoot))
	warnings := capture.AtLeast(slog.LevelWarn)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0].Message, "could not remove a name it was renamed from")
}
