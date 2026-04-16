package e2e

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

var errUnexpectedFetch = errors.New("unexpected fetch")

// Validates: R-6
func TestEnsureTestDriveMetadata_WritesMissingPersonalDriveMetadata(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	fetchCalls := 0
	err := ensureTestDriveMetadata(
		t.Context(),
		[]string{"personal:user@example.com"},
		func(context.Context, string) (*config.DriveMetadata, error) {
			fetchCalls++
			return &config.DriveMetadata{DriveID: "drive-123"}, nil
		},
	)
	require.NoError(t, err)
	assert.Equal(t, 1, fetchCalls)

	cid := driveid.MustCanonicalID("personal:user@example.com")
	meta, found, lookupErr := config.LookupDriveMetadata(cid)
	require.NoError(t, lookupErr)
	require.True(t, found)
	assert.Equal(t, "drive-123", meta.DriveID)
}

// Validates: R-6
func TestEnsureTestDriveMetadata_SkipsExistingMetadata(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cid := driveid.MustCanonicalID("business:user@example.com")
	require.NoError(t, config.SaveDriveMetadata(cid, &config.DriveMetadata{DriveID: "existing-drive"}))

	err := ensureTestDriveMetadata(
		t.Context(),
		[]string{cid.String()},
		func(context.Context, string) (*config.DriveMetadata, error) {
			t.Fatal("fetcher should not be called when metadata already exists")
			return nil, errUnexpectedFetch
		},
	)
	require.NoError(t, err)

	data, readErr := os.ReadFile(config.DriveMetadataPath(cid))
	require.NoError(t, readErr)
	assert.Contains(t, string(data), "existing-drive")
}

// Validates: R-6
func TestEnsureTestDriveMetadata_RejectsSharePointWithoutMetadata(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	err := ensureTestDriveMetadata(
		t.Context(),
		[]string{"sharepoint:user@example.com:site:library"},
		func(context.Context, string) (*config.DriveMetadata, error) {
			t.Fatal("fetcher should not be called for SharePoint metadata bootstrap")
			return nil, errUnexpectedFetch
		},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing drive metadata for SharePoint test drive")
}
