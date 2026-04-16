package cli

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func TestRemoveDriveDataFiles_BothExist(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	cid := driveid.MustCanonicalID("personal:user@example.com")

	// Create the retained state DB.
	statePath := config.DriveStatePath(cid)
	require.NotEmpty(t, statePath)
	require.NoError(t, os.WriteFile(statePath, []byte("fake-db"), 0o600))

	removed, err := removeDriveDataFiles(cid, testDriveLogger(t))
	require.NoError(t, err)
	assert.Equal(t, 1, removed)

	// The state DB should be gone.
	_, statErr := os.Stat(statePath)
	assert.True(t, os.IsNotExist(statErr), "state DB should be deleted")
}

func TestRemoveDriveDataFiles_OnlyStateDB(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	cid := driveid.MustCanonicalID("personal:user@example.com")

	// Create only state DB.
	statePath := config.DriveStatePath(cid)
	require.NoError(t, os.WriteFile(statePath, []byte("fake-db"), 0o600))

	removed, err := removeDriveDataFiles(cid, testDriveLogger(t))
	require.NoError(t, err)
	assert.Equal(t, 1, removed)

	_, statErr := os.Stat(statePath)
	assert.True(t, os.IsNotExist(statErr))
}

func TestRemoveDriveDataFiles_NeitherExists(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	cid := driveid.MustCanonicalID("personal:user@example.com")

	// No files on disk — should succeed idempotently.
	removed, err := removeDriveDataFiles(cid, testDriveLogger(t))
	require.NoError(t, err)
	assert.Equal(t, 0, removed)
}
