package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func TestDriveSession_Fields(t *testing.T) {
	// DriveSession should expose Client, Transfer, DriveID, Resolved.
	// TokenSource was removed — it was set but never read by any caller.
	var ds DriveSession
	assert.Nil(t, ds.Client)
	assert.Nil(t, ds.Transfer)
	assert.True(t, ds.DriveID.IsZero())
	assert.Nil(t, ds.Resolved)
}

func TestNewDriveSession_EmptyTokenPath(t *testing.T) {
	// When DriveTokenPath returns "" (zero canonical ID), constructor should error.
	resolved := &config.ResolvedDrive{
		CanonicalID: driveid.CanonicalID{}, // zero value
	}
	cfg := config.DefaultConfig()
	logger := buildLogger(nil, CLIFlags{})

	_, err := NewDriveSession(context.Background(), resolved, cfg, logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token path")
}

func TestNewDriveSession_NotLoggedIn(t *testing.T) {
	// When the token file doesn't exist, constructor should return a
	// user-friendly "not logged in" error.
	cid, err := driveid.NewCanonicalID("personal:nobody@example.com")
	require.NoError(t, err)

	resolved := &config.ResolvedDrive{
		CanonicalID: cid,
	}
	cfg := config.DefaultConfig()
	logger := buildLogger(nil, CLIFlags{})

	_, err = NewDriveSession(context.Background(), resolved, cfg, logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "login")
}

func TestDriveSession_DriveIDFromConfig(t *testing.T) {
	// Verify that a non-zero DriveID is correctly stored in the session struct.
	// The full constructor path (configured DriveID skips Drives() API call)
	// cannot be tested without a real token, so we verify struct population.
	driveID := driveid.New("abc123")
	assert.False(t, driveID.IsZero())

	session := &DriveSession{DriveID: driveID}
	assert.Equal(t, driveID, session.DriveID)
}
