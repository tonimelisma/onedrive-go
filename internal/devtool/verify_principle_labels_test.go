package devtool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.10.18
func TestFindPrincipleLabelViolations_FlagsALabelOutsideTheHistory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for path, content := range map[string]string{
		"internal/demo/demo.go":      "package demo\n\n// kept in the trash (P-4).\n",
		"spec/design/demo.md":        "A rule (P-6) and a requirement (R-2.17.6).\n",
		"spec/history/notes/plan.md": "The old label P-4 stays in the record.\n",
		"internal/demo/clean.go":     "package demo\n\n// kept in the trash (R-6.4.9).\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}

	violations, err := findPrincipleLabelViolations(root)
	require.NoError(t, err)

	locations := make([]string, 0, len(violations))
	for _, violation := range violations {
		locations = append(locations, violation.Location)
	}
	assert.ElementsMatch(t, []string{"internal/demo/demo.go:3", "spec/design/demo.md:1"}, locations)
}

// Validates: R-6.10.18
func TestFindPrincipleLabelViolations_RealRepoIsClean(t *testing.T) {
	violations, err := findPrincipleLabelViolations(repoRootForTest(t))
	require.NoError(t, err)
	assert.Empty(t, violations, "principle labels left in the tree")
}
