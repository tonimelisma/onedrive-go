package e2e

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

var errUnexpectedFetch = errors.New("unexpected fetch")

// Validates: R-6
func TestEnsureTestDriveIdentity_WritesMissingPersonalDriveIdentity(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	fetchCalls := 0
	err := ensureTestDriveIdentity(
		t.Context(),
		[]string{"personal:user@example.com"},
		func(context.Context, string) (*config.DriveIdentity, error) {
			fetchCalls++
			return &config.DriveIdentity{DriveID: "drive-123"}, nil
		},
	)
	require.NoError(t, err)
	assert.Equal(t, 1, fetchCalls)

	cid := driveid.MustCanonicalID("personal:user@example.com")
	identity, found, lookupErr := config.LookupDriveIdentity(cid)
	require.NoError(t, lookupErr)
	require.True(t, found)
	assert.Equal(t, "drive-123", identity.DriveID)
}

// Validates: R-6
func TestEnsureTestDriveIdentity_SkipsExistingIdentity(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cid := driveid.MustCanonicalID("business:user@example.com")
	require.NoError(t, config.SaveDriveIdentity(cid, &config.DriveIdentity{DriveID: "existing-drive"}))

	err := ensureTestDriveIdentity(
		t.Context(),
		[]string{cid.String()},
		func(context.Context, string) (*config.DriveIdentity, error) {
			t.Fatal("fetcher should not be called when drive identity already exists")
			return nil, errUnexpectedFetch
		},
	)
	require.NoError(t, err)

	identity, found, lookupErr := config.LookupDriveIdentity(cid)
	require.NoError(t, lookupErr)
	require.True(t, found)
	assert.Equal(t, "existing-drive", identity.DriveID)
}

// Validates: R-6
func TestEnsureTestDriveIdentity_RejectsSharePointWithoutIdentity(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	err := ensureTestDriveIdentity(
		t.Context(),
		[]string{"sharepoint:user@example.com:site:library"},
		func(context.Context, string) (*config.DriveIdentity, error) {
			t.Fatal("fetcher should not be called for SharePoint identity bootstrap")
			return nil, errUnexpectedFetch
		},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing drive identity for SharePoint test drive")
}
