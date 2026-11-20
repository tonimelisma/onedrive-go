package sync

import (
	"errors"
	"io/fs"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Validates: R-6.2.13
func TestFullScan_RootReadDirEIOReturnsError(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	tree := mustOpenSyncTree(t, syncRoot)
	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)

	var (
		events  []ChangeEvent
		jobs    []hashJob
		skipped []SkippedItem
	)
	walkFn := obs.makeWalkFunc(
		t.Context(), tree, map[string]bool{}, map[string]LocalStateRow{},
		&events, &jobs, &skipped, new(atomic.Int64), 0, map[string]struct{}{},
	)

	err := walkFn(syncRoot, nil, &fs.PathError{Op: "readdirent", Path: syncRoot, Err: errors.New("input/output error")})
	require.ErrorIs(t, err, ErrMountRootUnavailable, "an unreadable sync folder stops the scan")
	assert.Empty(t, skipped, "no read boundary at \".\" that would match no path")
}
