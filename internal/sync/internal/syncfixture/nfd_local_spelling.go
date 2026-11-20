package syncfixture

import (
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// DarwinGOOS is runtime.GOOS on macOS.
const DarwinGOOS = "darwin"

const (
	// NfdCafe is "café" spelled as e + U+0301 COMBINING ACUTE ACCENT.
	NfdCafe = "café"
	// NfcCafe is the same name, precomposed (U+00E9).
	NfcCafe = "café"
)

// SkipUnlessByteExactNames skips on macOS, whose volumes resolve either
// spelling to the same entry, so the scenario's premise does not hold.
func SkipUnlessByteExactNames(t *testing.T) {
	t.Helper()

	if runtime.GOOS == DarwinGOOS {
		t.Skip("macOS volumes are normalization-insensitive; both spellings name one entry")
	}
}

// LocalNames lists the raw names in dir, so a test sees the spelling on disk.
func LocalNames(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	return names
}
