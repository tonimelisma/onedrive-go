package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
)

func TestNewSyncEngine_EmptySyncDir(t *testing.T) {
	session := &driveops.Session{DriveID: driveid.New("abc123")}
	resolved := &config.ResolvedDrive{
		SyncDir:     "",
		CanonicalID: driveid.MustCanonicalID("personal:test@example.com"),
	}
	logger := buildLogger(nil, CLIFlags{})

	_, err := newSyncEngine(session, resolved, false, logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sync_dir not configured")
}

func TestNewSyncEngine_EmptyStatePath(t *testing.T) {
	session := &driveops.Session{DriveID: driveid.New("abc123")}
	// A zero CanonicalID produces empty StatePath.
	resolved := &config.ResolvedDrive{
		SyncDir:     "/tmp/sync",
		CanonicalID: driveid.CanonicalID{},
	}
	logger := buildLogger(nil, CLIFlags{})

	_, err := newSyncEngine(session, resolved, false, logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "state DB path")
}
