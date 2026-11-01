package config

import (
	"bytes"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverCIDFiles_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	ids, err := discoverCIDFiles(dir, slog.Default())
	require.NoError(t, err)
	assert.Nil(t, ids)
}

func TestDiscoverCIDFiles_EmptyDirString(t *testing.T) {
	ids, err := discoverCIDFiles("", slog.Default())
	require.NoError(t, err)
	assert.Nil(t, ids)
}

func TestDiscoverCIDFiles_SortedOutput(t *testing.T) {
	dir := t.TempDir()

	// Write files in reverse alphabetical order — output should be sorted.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "token_personal_z@example.com.json"), []byte(`{}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "token_personal_a@example.com.json"), []byte(`{}`), 0o600))

	ids, err := discoverCIDFiles(dir, slog.Default())
	require.NoError(t, err)
	require.Len(t, ids, 2)
	assert.Equal(t, "personal:a@example.com", ids[0].String())
	assert.Equal(t, "personal:z@example.com", ids[1].String())
}

func TestDiscoverFilesForEmail_EmptyInputs(t *testing.T) {
	dir := t.TempDir()

	paths, err := discoverFilesForEmail("", "state_", ".db", "a@b.com", slog.Default())
	require.NoError(t, err)
	assert.Nil(t, paths)
	paths, err = discoverFilesForEmail(dir, "state_", ".db", "", slog.Default())
	require.NoError(t, err)
	assert.Nil(t, paths)
}

// Validates: R-6.6.28
//
// A data directory that cannot be listed used to be reported as "no accounts",
// which sent the user to sign in again. It is now a Warn and an error naming
// the directory; only a directory that does not exist yet is an empty result.
func TestDiscoverCIDFiles_ReadDirError(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ids, err := discoverCIDFilesWithIO("/data", logger, configIO{
		readManagedDir: func(path string) ([]os.DirEntry, error) {
			return nil, fmt.Errorf("reading managed directory %s: %w", path, fs.ErrPermission)
		},
	})

	require.ErrorIs(t, err, fs.ErrPermission)
	assert.Contains(t, err.Error(), "cannot read the data directory /data")
	assert.Nil(t, ids)
	assert.Contains(t, logBuf.String(), "level=WARN")
	assert.Contains(t, logBuf.String(), "cannot read the data directory")

	logBuf.Reset()
	paths, err := discoverFilesForEmailWithIO("/data", "state_", ".db", "a@b.com", logger, configIO{
		readManagedDir: func(path string) ([]os.DirEntry, error) {
			return nil, fmt.Errorf("reading managed directory %s: %w", path, fs.ErrPermission)
		},
	})
	require.ErrorIs(t, err, fs.ErrPermission)
	assert.Nil(t, paths)
	assert.Contains(t, logBuf.String(), "level=WARN")
}

// Validates: R-6.6.28
func TestDiscoverCIDFiles_MissingDataDirIsNotAnError(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ids, err := discoverCIDFilesWithIO("/missing", logger, configIO{
		readManagedDir: func(path string) ([]os.DirEntry, error) {
			return nil, fmt.Errorf("opening managed directory %s: %w", path, fs.ErrNotExist)
		},
	})

	require.NoError(t, err)
	assert.Nil(t, ids)
	assert.NotContains(t, logBuf.String(), "level=WARN")
}

func TestContainsEmailBoundary_EdgeCases(t *testing.T) {
	tests := []struct {
		name   string
		fname  string
		needle string
		want   bool
	}{
		{"exact match at end", "state_personal_a@b.com.db", "_a@b.com", true},
		{"followed by underscore", "state_sharepoint_a@b.com_site_lib.db", "_a@b.com", true},
		{"substring collision", "state_personal_ba@b.com.db", "_a@b.com", false},
		{"no match at all", "state_personal_x@y.com.db", "_a@b.com", false},
		{"needle at very end (no suffix)", "account_personal_a@b.com", "_a@b.com", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, containsEmailBoundary(tt.fname, tt.needle))
		})
	}
}
