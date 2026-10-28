package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func fakeCopyFinishes(fake *fakeGraphDrive) func(context.Context, *CLIContext, *graph.Client, string) (string, error) {
	return func(_ context.Context, _ *CLIContext, _ *graph.Client, monitorURL string) (string, error) {
		return strings.TrimPrefix(monitorURL, fakeGraphCopyMonitorPrefix), nil
	}
}

func freshLeaf(t *testing.T, cmd *cobra.Command) *cobra.Command {
	t.Helper()

	cmd.SetContext(t.Context())
	return cmd
}

// Validates: R-1.8.2
func TestCpForce_FailedCopyRestoresTheOriginal(t *testing.T) {
	t.Parallel()

	cc, fake := jsonContractFileContext(t)
	fake.addFile("src.txt", "new")
	fake.addFile("dst.txt", "precious")
	fake.mu.Lock()
	originalID := fake.byPath("/dst.txt").ID
	fake.mu.Unlock()
	cc.seams.cpAwaitCopy = func(context.Context, *CLIContext, *graph.Client, string) (string, error) {
		// The monitor reports the copy failed (quota, a server fault, ...).
		return "", errors.New("copy failed")
	}

	_, err := runDetachedLeaf(t, freshLeaf(t, newCpCmd()), cc, "--force", "/src.txt", "/dst.txt")
	require.Error(t, err)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	original := fake.items[originalID]
	require.NotNil(t, original, "the original must not be deleted")
	assert.Equal(t, "dst.txt", original.Name, "and it gets its own name back")
	assert.Equal(t, "precious", string(original.Content))
	assert.Empty(t, fake.recycle, "nothing was deleted")
}

// Validates: R-1.8.2, R-1.8.3
func TestCpForce_SuccessReplacesAndKeepsSourceMtime(t *testing.T) {
	t.Parallel()

	cc, fake := jsonContractFileContext(t)
	fake.addFile("src.txt", "new")
	fake.addFile("dst.txt", "old")
	fake.mu.Lock()
	fake.byPath("/src.txt").Modified = "2020-05-06T07:08:09Z"
	fake.mu.Unlock()
	cc.seams.cpAwaitCopy = fakeCopyFinishes(fake)

	_, err := runDetachedLeaf(t, freshLeaf(t, newCpCmd()), cc, "--force", "/src.txt", "/dst.txt")
	require.NoError(t, err)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	copied := fake.byPath("/dst.txt")
	require.NotNil(t, copied)
	assert.Equal(t, "new", string(copied.Content))
	assert.Equal(t, "2020-05-06T07:08:09Z", copied.Modified, "the copy carries the source's modification time")
	require.Len(t, fake.recycle, 1, "the replaced original goes to the recycle bin")
	for _, recycled := range fake.recycle {
		assert.Equal(t, "old", string(recycled.Content))
	}
}

// Validates: R-1.7.4
func TestMv_RenameKeepsTheModificationTime(t *testing.T) {
	t.Parallel()

	cc, fake := jsonContractFileContext(t)
	fake.addFile("a.txt", "content")
	fake.mu.Lock()
	fake.byPath("/a.txt").Modified = "2005-05-05T05:05:05Z"
	fake.mu.Unlock()

	_, err := runDetachedLeaf(t, freshLeaf(t, newMvCmd()), cc, "/a.txt", "/b.txt")
	require.NoError(t, err)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	moved := fake.byPath("/b.txt")
	require.NotNil(t, moved)
	assert.Equal(t, "2005-05-05T05:05:05Z", moved.Modified, "a rename must not stamp its own time on the file")
}

// Validates: R-1.7.4, R-1.8.2
func TestMvForce_RestoredDestinationKeepsItsModificationTime(t *testing.T) {
	t.Parallel()

	cc, fake := jsonContractFileContext(t)
	fake.addFile("dst.txt", "precious")
	fake.mu.Lock()
	existing := fake.byPath("/dst.txt")
	existing.Modified = "2010-10-10T10:10:10Z"
	fake.mu.Unlock()
	session, err := cc.Session(t.Context())
	require.NoError(t, err)

	err = replaceDestination(t.Context(), cc, session, "/dst.txt",
		destInfo{
			parentID: fakeGraphRootID, newName: "dst.txt", existingID: existing.ID,
			existingModified: time.Date(2010, 10, 10, 10, 10, 10, 0, time.UTC),
		}, time.Now(),
		func() error { return errors.New("the move failed") })
	require.Error(t, err)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	restored := fake.byPath("/dst.txt")
	require.NotNil(t, restored, "the original gets its name back")
	assert.Equal(t, "2010-10-10T10:10:10Z", restored.Modified, "setting aside and restoring must keep its modification time")
}

// Validates: R-1.8.2
func TestMvForce_ReplacesWithoutDeletingFirst(t *testing.T) {
	t.Parallel()

	cc, fake := jsonContractFileContext(t)
	fake.addFile("a.txt", "new")
	fake.addFile("b.txt", "old")

	_, err := runDetachedLeaf(t, freshLeaf(t, newMvCmd()), cc, "--force", "/a.txt", "/b.txt")
	require.NoError(t, err)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	moved := fake.byPath("/b.txt")
	require.NotNil(t, moved)
	assert.Equal(t, "new", string(moved.Content))
	assert.Nil(t, fake.byPath("/a.txt"))
	require.Len(t, fake.recycle, 1)
}

// Validates: R-1.8.2
func TestReplaceDestination_CanceledOperationStillRestores(t *testing.T) {
	t.Parallel()

	cc, fake := jsonContractFileContext(t)
	fake.addFile("dst.txt", "precious")
	session, err := cc.Session(t.Context())
	require.NoError(t, err)
	fake.mu.Lock()
	existing := fake.byPath("/dst.txt")
	fake.mu.Unlock()

	ctx, cancel := context.WithCancel(t.Context())
	err = replaceDestination(ctx, cc, session, "/dst.txt",
		destInfo{parentID: fakeGraphRootID, newName: "dst.txt", existingID: existing.ID}, time.Now(),
		func() error {
			cancel() // Ctrl-C mid-copy
			return context.Canceled
		})
	require.ErrorIs(t, err, context.Canceled)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	assert.NotNil(t, fake.byPath("/dst.txt"), "a canceled command still puts the original back")
}

// Validates: R-1.8.2
func TestAsideNameFor_FitsTheNameLimit(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("é", 200) // 400 bytes
	name := asideNameFor(long, time.Unix(1, 0))
	assert.LessOrEqual(t, len(name), maxRemoteNameBytes)
	assert.True(t, strings.HasPrefix(name, asideNamePrefix))
	assert.True(t, strings.HasSuffix(name, "é"), "trimmed on a rune boundary")
}

// Validates: R-1.8.4
func TestCopyStatusOutcome(t *testing.T) {
	t.Parallel()

	for _, status := range []string{"inProgress", "notStarted", "waiting", "updating"} {
		done, err := copyStatusOutcome(status)
		assert.False(t, done, status)
		require.NoError(t, err)
	}
	done, err := copyStatusOutcome("completed")
	assert.True(t, done)
	require.NoError(t, err)
	for _, status := range []string{"failed", "deletePending", "deleteFailed", "somethingNew"} {
		done, err := copyStatusOutcome(status)
		assert.True(t, done, status)
		require.Error(t, err, status)
	}
}

// Validates: R-1.8.2
func TestCpForce_CleanupFailureWarnsAndSucceeds(t *testing.T) {
	t.Parallel()

	cc, fake := jsonContractFileContext(t)
	var logs bytes.Buffer
	cc.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	fake.addFile("src.txt", "new")
	fake.addFile("dst.txt", "old")
	fake.deleteRefused = true
	cc.seams.cpAwaitCopy = fakeCopyFinishes(fake)

	_, err := runDetachedLeaf(t, freshLeaf(t, newCpCmd()), cc, "--force", "/src.txt", "/dst.txt")
	require.NoError(t, err, "the replacement landed, so the command succeeded")

	assert.Contains(t, logs.String(), "the previous version is left on OneDrive as /"+asideNamePrefix)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	assert.Equal(t, "new", string(fake.byPath("/dst.txt").Content))
}

// Validates: R-1.8.2
func TestCpForce_HintsAboutAnInterruptedReplacement(t *testing.T) {
	t.Parallel()

	cc, fake := jsonContractFileContext(t)
	fake.addFile("src.txt", "new")
	leftover := asideNamePrefix + "1761400000000000000-dst.txt"
	fake.addFile(leftover, "the original")
	cc.seams.cpAwaitCopy = fakeCopyFinishes(fake)

	var stderr bytes.Buffer
	cmd := freshLeaf(t, newCpCmd())
	cc.OutputWriter = io.Discard
	cc.StatusWriter = &stderr
	cmd.SetContext(context.WithValue(t.Context(), cliContextKey{}, cc))
	cmd.SetArgs([]string{"--force", "/src.txt", "/dst.txt"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	require.NoError(t, cmd.Execute())

	assert.Contains(t, stderr.String(), "Note: /"+leftover+" is a previous version of dst.txt")
}

// Validates: R-1.8.2
func TestIsAsideNameFor(t *testing.T) {
	t.Parallel()

	assert.True(t, isAsideNameFor(asideNameFor("dst.txt", time.Unix(5, 0)), "dst.txt"))
	long := strings.Repeat("é", 200)
	assert.True(t, isAsideNameFor(asideNameFor(long, time.Unix(5, 0)), long), "a trimmed aside still matches")
	assert.False(t, isAsideNameFor(asideNameFor("other.txt", time.Unix(5, 0)), "dst.txt"))
	assert.False(t, isAsideNameFor(asideNamePrefix+"x-dst.txt", "dst.txt"), "the stamp is numeric")
	assert.False(t, isAsideNameFor("dst.txt", "dst.txt"))
}

// Validates: R-1.8.2
func TestReplaceDestination_RestoreIsBounded(t *testing.T) {
	t.Parallel()

	cc, fake := jsonContractFileContext(t)
	fake.addFile("dst.txt", "precious")
	fake.hangRenameTo = "dst.txt" // the network dies just as the original is put back
	cc.seams.asideRestoreTimeout = 100 * time.Millisecond
	session, err := cc.Session(t.Context())
	require.NoError(t, err)
	fake.mu.Lock()
	existing := fake.byPath("/dst.txt")
	fake.mu.Unlock()

	started := time.Now()
	err = replaceDestination(t.Context(), cc, session, "/dst.txt",
		destInfo{parentID: fakeGraphRootID, newName: "dst.txt", existingID: existing.ID}, time.Unix(7, 0),
		func() error { return errors.New("copy failed") })

	require.Error(t, err)
	assert.Contains(t, err.Error(), "is kept on OneDrive as", "the user is told where the original is")
	assert.Less(t, time.Since(started), 20*time.Second, "a hung restore must not hold the command")
}
