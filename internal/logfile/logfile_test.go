package logfile

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	require.NoError(t, os.WriteFile(path, []byte("existing content\n"), 0o644))

	f, err := Open(path, 0)
	require.NoError(t, err)

	_, writeErr := f.WriteString("new content\n")
	require.NoError(t, writeErr)
	require.NoError(t, f.Close())

	data, readErr := os.ReadFile(path)
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

func TestOpen_CleansOldFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Create an old log file (8 days ago).
	oldPath := filepath.Join(dir, "old.log")
	require.NoError(t, os.WriteFile(oldPath, []byte("old"), 0o644))
	oldTime := time.Now().Add(-8 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(oldPath, oldTime, oldTime))

	// Create a recent log file (1 day ago).
	recentPath := filepath.Join(dir, "recent.log")
	require.NoError(t, os.WriteFile(recentPath, []byte("recent"), 0o644))
	recentTime := time.Now().Add(-1 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(recentPath, recentTime, recentTime))

	// Open a new log file with 7-day retention.
	logPath := filepath.Join(dir, "current.log")
	f, err := Open(logPath, 7)
	require.NoError(t, err)
	defer f.Close()

	// Old file should be deleted, recent file should remain.
	_, oldErr := os.Stat(oldPath)
	assert.True(t, os.IsNotExist(oldErr), "old log file should be deleted")

	_, recentErr := os.Stat(recentPath)
	assert.NoError(t, recentErr, "recent log file should remain")
}

func TestOpen_ZeroRetentionSkipsCleanup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Create an old log file.
	oldPath := filepath.Join(dir, "old.log")
	require.NoError(t, os.WriteFile(oldPath, []byte("old"), 0o644))
	oldTime := time.Now().Add(-100 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(oldPath, oldTime, oldTime))

	// Open with 0 retention days — should not delete anything.
	logPath := filepath.Join(dir, "current.log")
	f, err := Open(logPath, 0)
	require.NoError(t, err)
	defer f.Close()

	_, oldErr := os.Stat(oldPath)
	assert.NoError(t, oldErr, "old log file should remain when retention is 0")
}

func TestOpen_RetentionIgnoresNonLogFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Create an old non-log file.
	txtPath := filepath.Join(dir, "data.txt")
	require.NoError(t, os.WriteFile(txtPath, []byte("data"), 0o644))
	oldTime := time.Now().Add(-100 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(txtPath, oldTime, oldTime))

	logPath := filepath.Join(dir, "current.log")
	f, err := Open(logPath, 1)
	require.NoError(t, err)
	defer f.Close()

	_, txtErr := os.Stat(txtPath)
	assert.NoError(t, txtErr, "non-.log files should not be deleted")
}
