package sync

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A file that stats but cannot be opened used to warn "hash computation
// failed" on every safety scan forever. It is now a skipped local_read_denied
// item (reported once, listed by status) and the scan logs nothing at WARN.
//
// Validates: R-6.6.30
func TestFullScan_UnreadableFileIsSkippedItemNotWarning(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "locked.txt", "secret")

	logger, logs := newCapturedLogger()
	obs := NewLocalObserver(emptyBaseline(), logger, 0)
	obs.hashFunc = func(context.Context, string) (string, error) {
		return "", fmt.Errorf("open locked.txt: %w", os.ErrPermission)
	}

	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err)

	require.Len(t, result.Skipped, 1)
	assert.Equal(t, "locked.txt", result.Skipped[0].Path)
	assert.Equal(t, IssueLocalReadDenied, result.Skipped[0].Reason)
	assert.Zero(t, countLevel(logs.String(), "WARN"))
}
