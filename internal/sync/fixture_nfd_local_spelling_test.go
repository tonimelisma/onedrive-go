package sync

import (
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// darwinGOOS is runtime.GOOS on macOS.
const darwinGOOS = "darwin"

const (
	// nfdCafe is "café" spelled as e + U+0301 COMBINING ACUTE ACCENT.
	nfdCafe = "café"
	// nfcCafe is the same name, precomposed (U+00E9).
	nfcCafe = "café"
)

// skipUnlessByteExactNames skips on macOS, whose volumes resolve either
// spelling to the same entry, so the scenario's premise does not hold.
func skipUnlessByteExactNames(t *testing.T) {
	t.Helper()

	if runtime.GOOS == darwinGOOS {
		t.Skip("macOS volumes are normalization-insensitive; both spellings name one entry")
	}
}

// localNames lists the raw names in dir, so a test sees the spelling on disk.
func localNames(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	return names
}
