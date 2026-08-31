package logfile

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// Validates: R-4.7.3
func TestOpen_CreatesFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "test.log")
	f, err := Open(path, 0)
	require.NoError(t, err)
	defer f.Close()

	_, statErr := os.Stat(path)
	assert.NoError(t, statErr, "log file should exist")
}

func TestOpen_CreatesParentDirs(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "deep", "nested", "dir", "test.log")
	f, err := Open(path, 0)
	require.NoError(t, err)
	defer f.Close()

	_, statErr := os.Stat(path)
	assert.NoError(t, statErr, "log file should exist after creating parent dirs")
}

func TestOpen_AppendsToExisting(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "append.log")
	require.NoError(t, os.WriteFile(path, []byte("existing content\n"), 0o600))

	f, err := Open(path, 0)
	require.NoError(t, err)

	_, writeErr := f.WriteString("new content\n")
	require.NoError(t, writeErr)
	require.NoError(t, f.Close())

	data, readErr := localpath.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, "existing content\nnew content\n", string(data))
}

func TestOpen_Permissions(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "perms.log")
	f, err := Open(path, 0)
	require.NoError(t, err)
	defer f.Close()

	info, statErr := os.Stat(path)
	require.NoError(t, statErr)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
}

// Validates: R-4.7.3
//
// log_file is an arbitrary path the user chooses and has no default, so the
// directory it lands in is very likely shared. Retention must delete this
// program's own logs and nothing else: a neighboring file it never created
// is not its to remove, however old.
func TestOpen_CleansOwnOldLogsOnly(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	stale := time.Now().Add(-8 * 24 * time.Hour)

	write := func(name string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
		require.NoError(t, os.Chtimes(path, stale, stale))

		return path
	}

	ownCurrent := write("current.log")
	ownRotation := write("current.log.1")
	ownDated := write("current-2026-01-01.log")
	foreign := write("backup.log")
	foreignPrefixed := write("current-other-app.txt")

	f, err := Open(filepath.Join(dir, "current.log"), 7)
	require.NoError(t, err)
	defer f.Close()

	// Reopening recreates the current log, so it exists but was emptied.
	info, err := os.Stat(ownCurrent)
	require.NoError(t, err)
	assert.Zero(t, info.Size(), "the stale current log is cleared by retention")

	for _, path := range []string{ownRotation, ownDated} {
		_, statErr := os.Stat(path)
		assert.True(t, os.IsNotExist(statErr), "own rotation %s should be deleted", path)
	}

	for _, path := range []string{foreign, foreignPrefixed} {
		_, statErr := os.Stat(path)
		assert.NoError(t, statErr, "%s belongs to something else and must survive", path)
	}
}

func TestOpen_RetentionLeavesNonExpiredFiles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		retention   int
		fixtureName string
		fixtureData []byte
		wantMessage string
	}{
		{
			name:        "ZeroRetentionSkipsCleanup",
			retention:   0,
			fixtureName: "old.log",
			fixtureData: []byte("old"),
			wantMessage: "old log file should remain when retention is 0",
		},
		{
			name:        "RetentionIgnoresNonLogFiles",
			retention:   1,
			fixtureName: "data.txt",
			fixtureData: []byte("data"),
			wantMessage: "non-.log files should not be deleted",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			fixturePath := filepath.Join(dir, tt.fixtureName)
			require.NoError(t, os.WriteFile(fixturePath, tt.fixtureData, 0o600))

			oldTime := time.Now().Add(-100 * 24 * time.Hour)
			require.NoError(t, os.Chtimes(fixturePath, oldTime, oldTime))

			logPath := filepath.Join(dir, "current.log")
			f, err := Open(logPath, tt.retention)
			require.NoError(t, err)
			defer f.Close()

			_, statErr := os.Stat(fixturePath)
			assert.NoError(t, statErr, tt.wantMessage)
		})
	}
}
