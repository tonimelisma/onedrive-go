package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// namesCoexist reports whether the test temp filesystem can hold two distinct
// files named a and b. Case-insensitive volumes fold differently (APFS also
// folds ß to ss), so tests probe the exact pair they need.
func namesCoexist(t *testing.T, a, b string) bool {
	t.Helper()

	probe := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(probe, a), []byte("a"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(probe, b), []byte("b"), 0o600))
	data, err := localpath.ReadFile(filepath.Join(probe, a))
	require.NoError(t, err)

	return string(data) == "a"
}

// skipUnlessNamesCoexist skips a test whose scenario needs both a and b as
// separate local files, which a case-insensitive volume cannot hold.
func skipUnlessNamesCoexist(t *testing.T, a, b string) {
	t.Helper()

	if !namesCoexist(t, a, b) {
		t.Skipf("skipping: this filesystem cannot hold %q and %q as separate files", a, b)
	}
}
