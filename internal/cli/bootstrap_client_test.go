package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.8.27
//
// Every Graph client a command builds takes its token source from the
// command's session runtime, which holds one per sign-in file. A direct
// graph.TokenSourceFromPath call in a command would start a second refresh
// chain for the same file, whose rotated refresh token the runtime's source
// never sees.
func TestCLIBootstrapClientsUseTheSessionRuntime(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	checked := 0

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}

		data, readErr := os.ReadFile(name) //nolint:gosec // package-relative test input
		require.NoError(t, readErr)

		checked++

		assert.Zero(t, strings.Count(string(data), "graph.TokenSourceFromPath("),
			"%s builds a token source outside the session runtime; use SessionRuntime.BootstrapClient", name)
	}

	assert.NotZero(t, checked, "the scan must find the package's production files")
}
