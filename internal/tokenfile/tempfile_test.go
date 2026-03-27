package tokenfile

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/tonimelisma/onedrive-go/internal/trustedpath"
)

func TestMarshalTokenFile(t *testing.T) {
	t.Parallel()

	data, err := marshalTokenFile(&oauth2.Token{
		AccessToken:  "access",
		RefreshToken: "refresh",
		TokenType:    "Bearer",
	})
	require.NoError(t, err)

	var file File
	require.NoError(t, json.Unmarshal(data, &file))
	require.NotNil(t, file.Token)
	assert.Equal(t, "access", file.Token.AccessToken)
}

func TestSetTempFilePermissions(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	file, err := os.CreateTemp(tempDir, "token-temp-*.json")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, file.Close())
	})

	require.NoError(t, setTempFilePermissions(file))

	info, err := file.Stat()
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(FilePerms), info.Mode().Perm())
}

func TestWriteTempFileData(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	file, err := os.CreateTemp(tempDir, "token-temp-*.json")
	require.NoError(t, err)
	require.NoError(t, writeTempFileData(file, []byte("token-data")))
	require.NoError(t, file.Close())

	data, err := trustedpath.ReadFile(file.Name())
	require.NoError(t, err)
	assert.Equal(t, []byte("token-data"), data)

	closedFile, err := os.CreateTemp(tempDir, "token-temp-closed-*.json")
	require.NoError(t, err)
	require.NoError(t, closedFile.Close())

	err = writeTempFileData(closedFile, []byte("x"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "writing")
}

func TestSyncTempFile(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	file, err := os.CreateTemp(tempDir, "token-temp-*.json")
	require.NoError(t, err)
	require.NoError(t, syncTempFile(file))
	require.NoError(t, file.Close())

	closedFile, err := os.CreateTemp(tempDir, "token-temp-closed-*.json")
	require.NoError(t, err)
	require.NoError(t, closedFile.Close())

	err = syncTempFile(closedFile)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "syncing")
}

func TestCloseTempFile_JoinsPriorError(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	file, err := os.CreateTemp(tempDir, "token-temp-*.json")
	require.NoError(t, err)
	require.NoError(t, file.Close())

	prior := errors.New("prior")
	err = closeTempFile(file, prior)
	require.Error(t, err)
	require.ErrorIs(t, err, prior)
	assert.Contains(t, err.Error(), "closing temp file")
}

func TestRemoveTempPath_JoinsPriorError(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	blocked := filepath.Join(tempDir, "blocked")
	require.NoError(t, os.Mkdir(blocked, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(blocked, "child"), []byte("x"), 0o600))

	prior := errors.New("prior")
	err := removeTempPath(blocked, prior)
	require.Error(t, err)
	require.ErrorIs(t, err, prior)
	assert.Contains(t, err.Error(), "removing temp file")

	assert.NoError(t, removeTempPath(filepath.Join(tempDir, "missing"), nil))
}

func TestRenameTempFile(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	src := filepath.Join(tempDir, "token.tmp")
	dst := filepath.Join(tempDir, "token.json")
	require.NoError(t, os.WriteFile(src, []byte("token"), 0o600))

	require.NoError(t, renameTempFile(src, dst))

	data, err := trustedpath.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, []byte("token"), data)

	err = renameTempFile(src, filepath.Join(tempDir, "other.json"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "renaming")
}
