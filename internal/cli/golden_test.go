package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func requireGoldenText(t *testing.T, name, actual string) {
	t.Helper()

	path := filepath.Join("testdata", name)
	if updateGoldenRequested() {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(actual), 0o600))
	}

	//nolint:gosec // Golden fixtures are fixed package-local testdata files.
	expected, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, normalizeGoldenText(string(expected)), normalizeGoldenText(actual))
}

func updateGoldenRequested() bool {
	for _, arg := range os.Args[1:] {
		if arg == "-update" {
			return true
		}
	}

	return false
}

func normalizeGoldenText(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}
