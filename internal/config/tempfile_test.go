package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoveTempPath(t *testing.T) {
	t.Parallel()

	prior := errors.New("prior")
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	require.NoError(t, os.Mkdir(blocked, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(blocked, "child"), []byte("x"), 0o600))

	err := removeTempPath(blocked, "config temp file", prior)
	require.Error(t, err)
	require.ErrorIs(t, err, prior)
	assert.Contains(t, err.Error(), "removing config temp file")

	assert.NoError(t, removeTempPath(filepath.Join(dir, "missing"), "config temp file", nil))
}

func TestCloseTempFile(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	file, err := os.CreateTemp(tempDir, "config-temp-*.tmp")
	require.NoError(t, err)
	require.NoError(t, file.Close())

	err = closeTempFile(file, "config temp file", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "closing config temp file")

	prior := errors.New("prior")
	err = closeTempFile(file, "config temp file", prior)
	require.Error(t, err)
	require.ErrorIs(t, err, prior)
	assert.Contains(t, err.Error(), "closing config temp file")
}

func TestChmodTrustedTempPath(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "config.tmp")
	require.NoError(t, os.WriteFile(path, []byte("cfg"), 0o600))
	require.NoError(t, chmodTrustedTempPath(path, 0o600, "config file"))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	err = chmodTrustedTempPath(filepath.Join(tempDir, "missing.tmp"), 0o600, "config file")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "setting config file permissions")
}

func TestRenameTrustedTempPath(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	src := filepath.Join(tempDir, "config.tmp")
	dst := filepath.Join(tempDir, "config.toml")
	require.NoError(t, os.WriteFile(src, []byte("cfg"), 0o600))

	require.NoError(t, renameTrustedTempPath(src, dst, "config file"))

	data, err := os.ReadFile(dst) //nolint:gosec // Test destination is created in t.TempDir and controlled by the test.
	require.NoError(t, err)
	assert.Equal(t, []byte("cfg"), data)

	err = renameTrustedTempPath(src, filepath.Join(tempDir, "other.toml"), "config file")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "renaming config file")
}
