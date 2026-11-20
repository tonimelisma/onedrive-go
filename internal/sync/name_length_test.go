package sync

import (
	"fmt"
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

// A single name at the root is held only to the path limit: 400 units pass,
// 401 are path_too_long with the path check's message, whatever the bytes.
//
// Validates: R-2.11.10, R-2.11.12
func TestShouldObserve_RootLevelNameMeetsOnlyThePathLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		label    string
		name     string
		tooLong  bool
		wantUnit int
	}{
		{label: "400 ASCII", name: strings.Repeat("a", maxOneDrivePathLength), wantUnit: 400},
		{label: "401 ASCII", name: strings.Repeat("a", maxOneDrivePathLength+1), tooLong: true, wantUnit: 401},
		// U+4E2D: 400 units are 1,200 bytes.
		{label: "400 BMP", name: strings.Repeat("\u4e2d", maxOneDrivePathLength), wantUnit: 400},
		// U+1F600: 200 of them are 400 units; one more letter is 401.
		{label: "401 units with emoji", name: strings.Repeat("\U0001F600", 200) + "a", tooLong: true, wantUnit: 401},
	}
	for _, tt := range tests {
		require.Equal(t, tt.wantUnit, oneDrivePathLength(tt.name), tt.label)
		skip := shouldObserveWithFilter(tt.name, tt.name, observedKindFile, ContentFilterConfig{}, nil, LocalObservationRules{})
		if !tt.tooLong {
			assert.Nil(t, skip, tt.label)
			continue
		}
		require.NotNil(t, skip, tt.label)
		assert.Equal(t, issuePathTooLong, skip.Reason, tt.label)
		assert.Equal(t, fmt.Sprintf("path length 401 exceeds %d-character limit", maxOneDrivePathLength), skip.Detail, tt.label)
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

	e, syncRoot := newConflictCopyExecutor(t)
	name := strings.Repeat("w", 251) + ".txt"

	writeExecTestFile(t, syncRoot, name, "first")
	first := e.ExecuteConflictCopy(t.Context(), conflictCopyAction(name))
	requireOutcomeSuccess(t, &first)
	writeExecTestFile(t, syncRoot, name, "second")
	second := e.ExecuteConflictCopy(t.Context(), conflictCopyAction(name))
	requireOutcomeSuccess(t, &second)

	assert.NotEqual(t, first.OldPath, second.OldPath)
	assert.LessOrEqual(t, len(second.OldPath), maxLocalNameBytes, "the second same-second copy fits NAME_MAX")
	assert.True(t, strings.HasSuffix(second.OldPath, ".conflict-20260115-123045-2.txt"), second.OldPath)
	requireExecFileContent(t, syncRoot, first.OldPath, "first")
	requireExecFileContent(t, syncRoot, second.OldPath, "second")
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
