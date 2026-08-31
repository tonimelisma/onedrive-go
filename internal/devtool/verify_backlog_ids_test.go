package devtool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func repoRootForTest(t *testing.T) string {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	return root
}

func writeBacklogFixture(t *testing.T, registry string, goSource string) string {
	t.Helper()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "spec", "reference"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "internal", "demo"), 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, retiredBacklogRegistryPath), []byte(registry), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "internal", "demo", "demo.go"), []byte(goSource), 0o600))

	return root
}

// Validates: R-6.10.18
func TestFindBacklogIDViolations_RegisteredIDIsAccepted(t *testing.T) {
	root := writeBacklogFixture(t,
		"| `B-021` | `internal/demo/demo.go` |\n",
		"package demo\n\n// hash-less items skip verification (B-021).\nconst A = 1\n")

	violations, err := findBacklogIDViolations(root)
	require.NoError(t, err)
	assert.Empty(t, violations)
}

// Validates: R-6.10.18
func TestFindBacklogIDViolations_UnregisteredIDFails(t *testing.T) {
	root := writeBacklogFixture(t,
		"| `B-021` | `internal/demo/demo.go` |\n",
		"package demo\n\n// prior art (B-021), brand new pointer (B-999).\nconst A = 1\n")

	violations, err := findBacklogIDViolations(root)
	require.NoError(t, err)
	require.Len(t, violations, 1)
	assert.Contains(t, violations[0].Detail, "B-999")
	assert.Contains(t, violations[0].Location, "demo.go")
}

// Validates: R-6.10.18
func TestFindBacklogIDViolations_IdentifierFormIsRejected(t *testing.T) {
	root := writeBacklogFixture(t,
		"| `B-021` | `internal/demo/demo.go` |\n",
		"package demo\n\n// prior art (B-021).\nfunc TestExecutor_Upload_B068_Filled() {}\n")

	violations, err := findBacklogIDViolations(root)
	require.NoError(t, err)
	require.Len(t, violations, 1)
	assert.Contains(t, violations[0].Detail, "B068")
}

// Validates: R-6.10.18
func TestFindBacklogIDViolations_DeadRegistryEntryFails(t *testing.T) {
	root := writeBacklogFixture(t,
		"| `B-021` | `internal/demo/demo.go` |\n| `B-777` | `gone.go` |\n",
		"package demo\n\n// hash-less items skip verification (B-021).\nconst A = 1\n")

	violations, err := findBacklogIDViolations(root)
	require.NoError(t, err)
	require.Len(t, violations, 1)
	assert.Contains(t, violations[0].Detail, "B-777")
}

// Validates: R-6.10.18
func TestFindBacklogIDViolations_RealRepoIsClean(t *testing.T) {
	violations, err := findBacklogIDViolations(repoRootForTest(t))
	require.NoError(t, err)
	assert.Empty(t, violations, "retired backlog ID registry is out of sync with code")
}
