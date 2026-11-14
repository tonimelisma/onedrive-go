package synctest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The test body runs in a child test binary whose umask is the one asked
// for, so a folder created there with 0777 comes out as 0777 minus it.
//
// Validates: R-5.10.2
func TestRunUnderUmask_RunsTheTestUnderTheUmask(t *testing.T) {
	t.Parallel()
	if !RunUnderUmask(t, GroupWritableUmask) {
		return
	}

	dir := filepath.Join(t.TempDir(), "made")
	require.NoError(t, os.Mkdir(dir, 0o777)) //nolint:gosec // the requested mode is the point; the umask decides the result
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o775), info.Mode().Perm())
}
