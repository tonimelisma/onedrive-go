package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Validates: R-2.11.10
func TestOneDrivePathLength_CountsUTF16CodeUnits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want int
	}{
		{name: "ascii", path: "docs/a.txt", want: 10},
		{name: "finnish", path: strings.Repeat("\u00e4", 300), want: 300},
		{name: "cjk", path: strings.Repeat("\u65e5", 150), want: 150},
		{name: "emoji is two units", path: "\U0001F600", want: 2},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, oneDrivePathLength(tt.path), tt.name)
	}
}

// Validates: R-2.11.10
func TestFullScan_LongNonASCIIPathWithinTheCharacterLimitIsObserved(t *testing.T) {
	t.Parallel()

	// 150 CJK characters in two segments: 450 UTF-8 bytes, 151 characters.
	dir := t.TempDir()
	rel := filepath.Join(strings.Repeat("\u65e5", 75), strings.Repeat("\u672c", 75))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(rel)), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, rel), []byte("x"), 0o600))

	obs := newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err)

	assert.NotNil(t, findEvent(result.Events, filepath.ToSlash(rel)))
	assert.Empty(t, result.Skipped)
}

// Validates: R-2.11.11
func TestConflictCopyPath_OrdinalAndLongExtensionStayWithinNameMax(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 1, 15, 12, 30, 45, 0, time.UTC)

	long := strings.Repeat("w", 251) + ".txt" // 255 bytes
	second := filepath.Base(conflictCopyPathWithOrdinal("/sync/root/"+long, ts, 2))
	assert.LessOrEqual(t, len(second), maxLocalNameBytes, "the -2 copy must still be writable")
	assert.True(t, strings.HasSuffix(second, ".conflict-20260115-123045-2.txt"), second)

	longExt := "a." + strings.Repeat("e", 240)
	copied := filepath.Base(conflictCopyPathWithOrdinal("/sync/root/"+longExt, ts, 3))
	assert.LessOrEqual(t, len(copied), maxLocalNameBytes)
	assert.Contains(t, copied, ".conflict-20260115-123045-3")
}

// Validates: R-2.11.11
func TestExecutor_SecondSameSecondConflictCopyOfALongNameIsWritable(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	cfg.SetNowFunc(func() time.Time { return time.Date(2026, 1, 15, 12, 30, 45, 0, time.UTC) })
	e := newExecution(cfg, emptyBaseline())

	name := strings.Repeat("w", 251) + ".txt"
	abs := filepath.Join(syncRoot, name)

	first, firstRel, err := e.reserveUniqueConflictCopyPath(abs)
	require.NoError(t, err)
	second, secondRel, err := e.reserveUniqueConflictCopyPath(abs)
	require.NoError(t, err, "a second copy in the same second used to exceed NAME_MAX")

	assert.NotEqual(t, first, second)
	assert.FileExists(t, filepath.Join(syncRoot, firstRel))
	assert.FileExists(t, filepath.Join(syncRoot, secondRel))
}

// Validates: R-2.11.11
func TestTruncateUTF8_CutsAtABoundaryAndNeverEmpties(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{name: "fits", in: "abc", n: 5, want: "abc"},
		{name: "ascii", in: "abcdef", n: 3, want: "abc"},
		{name: "inside a rune", in: "a\u65e5b", n: 2, want: "a"},
		{name: "invalid byte before the cut", in: "a\xffbcdef", n: 4, want: "a\xffbc"},
		{name: "first rune longer than n", in: "\u65e5\u672c", n: 1, want: "\u65e5"},
		{name: "no room at all", in: "abc", n: 0, want: "a"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, truncateUTF8(tt.in, tt.n), tt.name)
	}
}
