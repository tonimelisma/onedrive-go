package main

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
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	cid := driveid.MustCanonicalID("personal:user@example.com")

	// Create both state DB and drive metadata files.
	statePath := config.DriveStatePath(cid)
	require.NotEmpty(t, statePath)
	require.NoError(t, os.WriteFile(statePath, []byte("fake-db"), 0o600))

	metaPath := config.DriveMetadataPath(cid)
	require.NotEmpty(t, metaPath)
	require.NoError(t, os.WriteFile(metaPath, []byte(`{"drive_id":"d1"}`), 0o600))

	removed, err := removeDriveDataFiles(cid, testDriveLogger(t))
	require.NoError(t, err)
	assert.Equal(t, 2, removed)

	// Both files should be gone.
	_, statErr := os.Stat(statePath)
	assert.True(t, os.IsNotExist(statErr), "state DB should be deleted")
	_, metaErr := os.Stat(metaPath)
	assert.True(t, os.IsNotExist(metaErr), "drive metadata should be deleted")
}

func TestRemoveDriveDataFiles_OnlyStateDB(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

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
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	cid := driveid.MustCanonicalID("personal:user@example.com")

	// No files on disk — should succeed idempotently.
	removed, err := removeDriveDataFiles(cid, testDriveLogger(t))
	require.NoError(t, err)
	assert.Equal(t, 0, removed)
}
