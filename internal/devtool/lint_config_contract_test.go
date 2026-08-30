package devtool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.2.1
//
// CLAUDE.md requires checking Close() on anything written to, because Close
// flushes buffers and a dropped error there is silent data loss. That rule is
// enforced only by errcheck, and errcheck once excluded every source line
// containing "defer " -- which switched the rule off entirely while the
// codebase appeared to comply.
//
// The exclusion is now scoped to test files. Nothing but this test stops the
// scope from being widened back, and the failure would be silent: production
// write-path closes would simply stop being reported.
func TestGolangciConfig_DeferErrcheckExclusionStaysScopedToTests(t *testing.T) {
	t.Parallel()

	root := repoRootFromDevtoolPackage(t)
	//nolint:gosec // reads a fixed repo-relative config path.
	raw, err := os.ReadFile(filepath.Join(root, ".golangci.yml"))
	require.NoError(t, err)

	lines := strings.Split(string(raw), "\n")

	found := 0

	for i, line := range lines {
		if !strings.Contains(line, `source:`) || !strings.Contains(line, "defer") {
			continue
		}

		found++

		// Walk back to the start of this exclusion entry and require that it
		// scopes itself to test files.
		scoped := false

		for j := i; j >= 0 && j > i-8; j-- {
			if strings.Contains(lines[j], `path: _test\.go`) {
				scoped = true

				break
			}
			if strings.Contains(lines[j], "- linters:") && j != i {
				break
			}
		}

		assert.Truef(t, scoped,
			"errcheck defer exclusion at .golangci.yml:%d is not scoped to _test.go; "+
				"widening it silently disables write-path Close() checking in production", i+1)
	}

	require.Positivef(t, found,
		"expected a scoped defer exclusion to exist; if it was removed entirely, delete this test deliberately")
}

func repoRootFromDevtoolPackage(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	require.NoError(t, err)

	return filepath.Dir(filepath.Dir(wd))
}
