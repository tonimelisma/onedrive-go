package sync

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// Validates: R-5.8.1, R-5.8.2
func TestRunOnce_AcceptedServerTransformIsNotDownloadedAgain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fileName string
		reported string // the content OneDrive's size and hash describe
		served   string // the bytes it actually serves
	}{
		// iOS HEIC: metadata describes the original, OneDrive serves a
		// smaller post-processed file.
		{name: "HEIC served smaller", fileName: "IMG_0001.HEIC", reported: "original heic bytes, larger", served: "processed"},
		// Deterministic same-size transform (AIP-protected Office files).
		{name: "same bytes at the reported size", fileName: "plan.docx", reported: "unprotected", served: "protected!!"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
			remote.addFile(t, "item-1", "root", tt.fileName, tt.reported)
			remote.contents["item-1"] = tt.served

			eng, syncRoot := newTestEngine(t, remote.client(t))
			first := runScenarioPass(t, eng)
			require.Equal(t, 1, first.Downloads)

			got, err := localpath.ReadFile(filepath.Join(syncRoot, tt.fileName))
			require.NoError(t, err)
			assert.Equal(t, tt.served, string(got))

			// The baseline used to record the local hash as the remote one
			// while remote_state kept OneDrive's, so every pass saw a remote
			// change and downloaded the file again (three times each).
			second := runScenarioPass(t, eng)
			assert.Zero(t, second.Downloads+second.Uploads, "an accepted transform is converged")
		})
	}
}

// Validates: R-5.6.12
func TestUploadTransferError_InvalidatedSessionIsAStalePlan(t *testing.T) {
	t.Parallel()

	err := uploadTransferError(fmt.Errorf("uploading a.bin: %w", driveops.ErrUploadSessionInvalidated))

	// Another writer changed the item mid-upload: re-observe and replan,
	// never upload over the change.
	require.ErrorIs(t, err, errActionPreconditionChanged)
	assert.Equal(t, resultSuperseded, classifyResult(&actionCompletion{Err: err}).Class)
}
