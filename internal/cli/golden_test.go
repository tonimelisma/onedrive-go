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

	assertGoldenFile(t, name, []byte(normalizeGoldenText(actual)))
}

func normalizeGoldenText(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

const (
	updateGoldenFlagName = "update"
)

func updateGoldenEnabled() bool {
	for _, arg := range os.Args[1:] {
		if arg == "-"+updateGoldenFlagName {
			return true
		}
	}

	return false
}

func assertGoldenFile(t *testing.T, name string, actual []byte) {
	t.Helper()

	path := filepath.Clean(filepath.Join("testdata", name))
	require.True(
		t,
		path == "testdata" || strings.HasPrefix(path, "testdata"+string(os.PathSeparator)),
		"golden path must stay under testdata",
	)

	if updateGoldenEnabled() {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, actual, 0o600))
	}

	expected, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(expected), string(actual))
}
