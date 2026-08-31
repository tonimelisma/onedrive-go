package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// localDeleteAction builds a local-delete action whose baseline carries no
// local hash, which is what the scanner records when hashing a file fails.
func localDeleteAction(path string, entry *BaselineEntry) *Action {
	return &Action{
		Type:   ActionLocalDelete,
		Path:   path,
		ItemID: "item1",
		View:   &pathView{Baseline: entry},
	}
}

func statMtime(t *testing.T, path string) int64 {
	t.Helper()

	info, err := os.Stat(path)
	require.NoError(t, err)

	return info.ModTime().UnixNano()
}

// Validates: R-6.2.4
//
// A baseline row can carry no local hash: the scanner records an empty hash
// when hashing fails. Skipping verification there deletes local content
// without ever checking whether the user changed it since, which is the
// destructive half of the case S4 exists to prevent.
func TestExecutor_LocalDelete_NoBaselineHashAndChangedFile_PreservesLocalContent(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-unhashed.txt", "content the user extended after the failed hash")

	action := localDeleteAction("exec-unhashed.txt", &BaselineEntry{
		LocalHash:      "",
		LocalSize:      4,
		LocalSizeKnown: true,
		LocalMtime:     1,
	})

	o := e.ExecuteLocalDelete(t.Context(), action)

	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
	assert.Equal(t, permissionCapabilityLocalWrite, o.FailureCapability)

	contents, err := localpath.ReadFile(filepath.Join(syncRoot, "exec-unhashed.txt"))
	require.NoError(t, err)
	assert.Equal(t, "content the user extended after the failed hash", string(contents),
		"an unverifiable local delete must keep the local file")
}

// Validates: R-6.2.4
//
// The guard must not strand deletions. When the baseline records size and
// mtime and the file still matches both, there is positive evidence the file
// is untouched and the delete proceeds.
func TestExecutor_LocalDelete_NoBaselineHashAndUnchangedFile_Deletes(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-unhashed-clean.txt", "same")
	absPath := filepath.Join(syncRoot, "exec-unhashed-clean.txt")

	action := localDeleteAction("exec-unhashed-clean.txt", &BaselineEntry{
		LocalHash:      "",
		LocalSize:      int64(len("same")),
		LocalSizeKnown: true,
		LocalMtime:     statMtime(t, absPath),
	})

	o := e.ExecuteLocalDelete(t.Context(), action)
	requireOutcomeSuccess(t, &o)

	_, statErr := os.Stat(absPath)
	assert.True(t, os.IsNotExist(statErr), "an unchanged file should still be deleted")
}

// Validates: R-6.2.4
//
// With neither a hash nor a recorded size there is no evidence at all, and a
// destructive action may not proceed on no evidence.
func TestExecutor_LocalDelete_NoBaselineEvidence_PreservesLocalContent(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	writeExecTestFile(t, syncRoot, "exec-no-evidence.txt", "irreplaceable")

	action := localDeleteAction("exec-no-evidence.txt", &BaselineEntry{})

	o := e.ExecuteLocalDelete(t.Context(), action)

	require.ErrorIs(t, o.Error, errActionPreconditionChanged)

	contents, err := localpath.ReadFile(filepath.Join(syncRoot, "exec-no-evidence.txt"))
	require.NoError(t, err)
	assert.Equal(t, "irreplaceable", string(contents))
}
