package cli

import (
	"flag"
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

// updateGolden registers the -update flag CLAUDE.md documents for refreshing
// golden files.
//
// It has to be a real registered flag: the testing package parses flags before
// any test runs and rejects unknown ones, so scanning os.Args by hand meant
// `go test -update` failed with "flag provided but not defined" and the
// documented workflow never worked.
//
//nolint:gochecknoglobals // flag registration is package-level by definition.
var updateGolden = flag.Bool("update", false, "rewrite golden files from current output")

func updateGoldenEnabled() bool {
	return *updateGolden
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
