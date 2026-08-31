package devtool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeMutexFixture(t *testing.T, source string) string {
	t.Helper()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "internal", "demo"), 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "internal", "demo", "demo.go"), []byte(source), 0o600))

	return root
}

// Validates: R-6.3.5
func TestFindMutexDocViolations_UndocumentedFieldIsReported(t *testing.T) {
	root := writeMutexFixture(t, `package demo

type box struct {
	mu    sync.Mutex
	state int
}
`)

	violations, err := findMutexDocViolations(root)
	require.NoError(t, err)
	require.Len(t, violations, 1)
	assert.Contains(t, violations[0].Detail, "does not say what it guards")
}

// Validates: R-6.3.5
func TestFindMutexDocViolations_CommentAboveOrInlineSatisfiesTheRule(t *testing.T) {
	root := writeMutexFixture(t, `package demo

type box struct {
	// mu guards state.
	mu    sync.Mutex
	state int
}

type other struct {
	mu    sync.RWMutex // guards value
	value int
}

func (b *box) use() {
	b.mu.Lock()
	defer b.mu.Unlock()
}
`)

	violations, err := findMutexDocViolations(root)
	require.NoError(t, err)
	assert.Empty(t, violations)
}

// Validates: R-6.3.5
//
// Locals matter as much as fields: a mutex guarding accumulator slices inside
// a parallel walk is exactly as opaque as a struct field.
func TestFindMutexDocViolations_UndocumentedLocalIsReported(t *testing.T) {
	root := writeMutexFixture(t, `package demo

func walk() {
	var mu stdsync.Mutex
	var rows []int
	_ = mu
	_ = rows
}
`)

	violations, err := findMutexDocViolations(root)
	require.NoError(t, err)
	require.Len(t, violations, 1)
	assert.Contains(t, violations[0].Location, "demo.go")
}

// Validates: R-6.3.5
func TestFindMutexDocViolations_RealRepoIsClean(t *testing.T) {
	violations, err := findMutexDocViolations(repoRootForTest(t))
	require.NoError(t, err)
	assert.Empty(t, violations, "every mutex must state what it guards")
}
