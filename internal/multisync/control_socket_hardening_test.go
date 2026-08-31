package multisync

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Validates: R-6.3.3
//
// The socket file is 0600, so other users cannot connect. What that does not
// stop is replacement: anyone who can create entries in the directory can
// unlink the socket and bind their own, and the CLI then talks to them. A
// world-writable directory without the sticky bit is exactly that, and
// MkdirAll will not tighten one that already exists.
func TestStartControlSocketServer_RefusesAWritableDirectory(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "preexisting")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	// Loosened deliberately: this is the attacker-created directory the guard
	// exists to refuse, so the test has to produce one.
	require.NoError(t, os.Chmod(dir, 0o777)) //nolint:gosec // the permissive mode is the condition under test

	_, err := startControlSocketServer(
		t.Context(),
		filepath.Join(dir, "control.sock"),
		http.NewServeMux(),
		synctest.TestLogger(t),
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "writable by other users")
}

// Validates: R-6.3.3
//
// The guard is checked directly here rather than by binding a socket: a
// temporary directory path on macOS already exceeds the Unix socket length
// limit, which is the very constraint the temp-directory fallback exists for.
func TestAssertControlSocketDirSafe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mode      os.FileMode
		wantError bool
	}{
		{name: "OwnerOnly", mode: 0o700},
		{name: "GroupAndWorldReadable", mode: 0o755},
		{name: "GroupWritable", mode: 0o770, wantError: true},
		{name: "WorldWritable", mode: 0o777, wantError: true},
		{name: "WorldWritableSticky", mode: 0o777 | os.ModeSticky},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := filepath.Join(t.TempDir(), "runtime")
			require.NoError(t, os.MkdirAll(dir, 0o700))
			require.NoError(t, os.Chmod(dir, tt.mode))

			err := assertControlSocketDirSafe(dir)
			if !tt.wantError {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), "writable by other users")
		})
	}
}

// Validates: R-6.3.3
//
// The system temp directory is the case the sticky bit exists for: shared and
// world-writable, but others cannot remove entries they do not own. Refusing
// it would refuse the fallback path the product actually uses.
func TestAssertControlSocketDirSafe_AcceptsTheSystemTempDirectory(t *testing.T) {
	t.Parallel()

	require.NoError(t, assertControlSocketDirSafe(os.TempDir()))
}

// Validates: R-6.3.3
//
// A conventional 0755 data directory must keep working; readability by others
// is not what allows a socket to be replaced.
func TestAssertControlSocketDirSafe_AcceptsAConventionalDataDirectory(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "data")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	// 0755 is the conventional data-directory mode this must keep accepting.
	require.NoError(t, os.Chmod(dir, 0o755)) //nolint:gosec // the mode is the condition under test

	require.NoError(t, assertControlSocketDirSafe(dir))
}

// Validates: R-6.3.3
func TestAssertControlSocketDirSafe_MissingDirectoryIsReported(t *testing.T) {
	t.Parallel()

	err := assertControlSocketDirSafe(filepath.Join(t.TempDir(), "absent"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stat control socket directory")
}
