package syncexec

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
)

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

	syncfixture.WriteExecTestFile(t, syncRoot, name, "first")
	first := e.ExecuteConflictCopy(t.Context(), conflictCopyAction(name))
	syncfixture.RequireOutcomeSuccess(t, &first)
	syncfixture.WriteExecTestFile(t, syncRoot, name, "second")
	second := e.ExecuteConflictCopy(t.Context(), conflictCopyAction(name))
	syncfixture.RequireOutcomeSuccess(t, &second)

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
