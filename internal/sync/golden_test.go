package sync

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const syncUpdateGoldenFlagName = "update"

func TestMain(m *testing.M) {
	flag.Bool(syncUpdateGoldenFlagName, false, "update sync golden files")
	flag.Parse()
	os.Exit(m.Run())
}

func syncUpdateGoldenEnabled() bool {
	if registered := flag.Lookup(syncUpdateGoldenFlagName); registered != nil && registered.Value.String() == "true" {
		return true
	}
	for _, arg := range os.Args[1:] {
		if arg == "-"+syncUpdateGoldenFlagName {
			return true
		}
	}

	return false
}

func assertSyncGoldenFile(t *testing.T, name string, actual []byte) {
	t.Helper()

	path := filepath.Clean(filepath.Join("testdata", name))
	require.True(
		t,
		path == "testdata" || strings.HasPrefix(path, "testdata"+string(os.PathSeparator)),
		"golden path must stay under testdata",
	)

	if syncUpdateGoldenEnabled() {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, actual, 0o600))
	}

	expected, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(expected), string(actual))
}
