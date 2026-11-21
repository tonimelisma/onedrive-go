package syncobserve

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// A file that stats but cannot be opened used to warn "hash computation
// failed" on every safety scan forever. It is now a skipped local_read_denied
// item (reported once, listed by status) and the scan logs nothing at WARN.
//
// Validates: R-6.6.30
func TestFullScan_UnreadableFileIsSkippedItemNotWarning(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	syncfixture.WriteTestFile(t, dir, "locked.txt", "secret")

	logger, logs := syncfixture.NewCapturedLogger()
	obs := NewLocalObserver(syncfixture.EmptyBaseline(), logger, 0)
	obs.hooks.HashFile = func(context.Context, string) (string, error) {
		return "", fmt.Errorf("open locked.txt: %w", os.ErrPermission)
	}

	result, err := obs.FullScan(t.Context(), syncfixture.MustOpenSyncTree(t, dir))
	require.NoError(t, err)

	require.Len(t, result.Skipped, 1)
	assert.Equal(t, "locked.txt", result.Skipped[0].Path)
	assert.Equal(t, syncdomain.IssueLocalReadDenied, result.Skipped[0].Reason)
	assert.Zero(t, syncfixture.CountLevel(logs.String(), "WARN"))
}
