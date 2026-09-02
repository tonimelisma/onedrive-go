package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func sharedFolderCID(t *testing.T) driveid.CanonicalID {
	t.Helper()

	cid, err := driveid.ConstructShared("alice@example.com", "srcdrive", "srcitem")
	require.NoError(t, err)

	return cid
}

// Validates: R-2.3.8
//
// A separately configured shared folder is its own drive, not a scope nested
// inside the drive that happens to share it. The projection kind is what keeps
// its issues grouped under itself in status output rather than folded into a
// parent's scopes.
func TestBuildConfiguredStatusDrive_StandaloneSharedFolderIsItsOwnDrive(t *testing.T) {
	t.Parallel()

	cid := sharedFolderCID(t)
	drive := &config.Drive{SyncDir: "/tmp/shared-root", Owner: "Alice Smith"}

	status := buildConfiguredStatusDrive(t.Context(), &config.Config{}, cid, drive, nil, nil)

	assert.Equal(t, statusProjectionStandalone, status.ProjectionKind,
		"a separately configured shared folder is a standalone drive, not a nested child projection")
	assert.Equal(t, cid.String(), status.MountID,
		"its scopes group under its own mount, so the mount identity must be the drive's own")
	assert.Equal(t, cid.String(), status.CanonicalID)
}

// Validates: R-2.3.9
//
// The canonical ID of a shared folder encodes the source drive and item, which
// means nothing to the person reading a failure. The configured owner is the
// identity they recognize.
func TestBuildConfiguredStatusDrive_StandaloneSharedFolderShowsUserFacingIdentity(t *testing.T) {
	t.Parallel()

	cid := sharedFolderCID(t)
	drive := &config.Drive{SyncDir: "/tmp/shared-root", Owner: "Alice Smith"}

	status := buildConfiguredStatusDrive(t.Context(), &config.Config{}, cid, drive, nil, nil)

	assert.Equal(t, "Alice Smith", status.Name)
	assert.Equal(t, "Alice Smith", status.DisplayName)
	assert.NotContains(t, status.Name, "srcdrive",
		"the source drive id is an internal identifier and must not surface as the name")
	assert.NotContains(t, status.Name, "srcitem")
}

// Validates: R-2.3.9
//
// With no configured owner the fallback still has to be readable rather than
// the raw canonical ID.
func TestBuildConfiguredStatusDrive_SharedFolderWithoutOwnerFallsBackToReadableName(t *testing.T) {
	t.Parallel()

	cid := sharedFolderCID(t)

	status := buildConfiguredStatusDrive(t.Context(), &config.Config{}, cid, &config.Drive{SyncDir: "/tmp/x"}, nil, nil)

	assert.NotEmpty(t, status.Name)
	assert.NotEqual(t, cid.String(), status.Name,
		"falling back to the canonical ID would surface an opaque internal identifier")
}
