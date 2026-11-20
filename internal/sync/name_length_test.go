package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Validates: R-2.11.10
func TestFullScan_LongNonASCIIPathWithinTheCharacterLimitIsObserved(t *testing.T) {
	t.Parallel()

	// 150 CJK characters in two segments: 450 UTF-8 bytes, 151 characters.
	dir := t.TempDir()
	rel := filepath.Join(strings.Repeat("\u65e5", 75), strings.Repeat("\u672c", 75))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(rel)), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, rel), []byte("x"), 0o600))

	obs := syncobserve.NewLocalObserver(syncfixture.EmptyBaseline(), synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), syncfixture.MustOpenSyncTree(t, dir))
	require.NoError(t, err)

	assert.NotNil(t, syncfixture.FindEvent(result.Events, filepath.ToSlash(rel)))
	assert.Empty(t, result.Skipped)
}
