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

const (
	updateGoldenFlagName    = "update"
	updateGoldenFlagEnabled = "true"
)

func updateGoldenEnabled() bool {
	updateFlag := flag.Lookup(updateGoldenFlagName)
	if updateFlag == nil {
		flag.Bool(updateGoldenFlagName, false, "update golden files")
		updateFlag = flag.Lookup(updateGoldenFlagName)
	}

	return updateFlag != nil && updateFlag.Value.String() == updateGoldenFlagEnabled
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
