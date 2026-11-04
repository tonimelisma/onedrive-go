package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.10.13
//
// A folder that can be listed but not written (mode 0555) refuses every
// download into it the same way. That is the directory's condition, with a
// timed write trial, not a per-file denial revisited only by each file's own
// retry: live on 2026-11-03 such a download was retried every 50 seconds with
// no scope and no issue. A read denial inside the same folder is still the
// file's own.
func TestPermHandler_HandleLocalPermission_UnwritableParentIsAWriteBoundary(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere; the probe cannot see the denial")
	}

	ph, syncRoot := newTestPermHandler(t, nil)
	subDir := filepath.Join(syncRoot, "ro")
	require.NoError(t, os.MkdirAll(subDir, 0o750))
	require.NoError(t, os.Chmod(subDir, 0o555)) //nolint:gosec // a listable, unwritable folder is the case under test
	t.Cleanup(func() {
		assert.NoError(t, os.Chmod(subDir, 0o700)) //nolint:gosec // restoring the folder so the temp dir can be removed
	})

	write := &actionCompletion{
		Path:              "ro/into-ro.bin",
		ActionType:        ActionDownload,
		FailureCapability: permissionCapabilityLocalWrite,
		ErrMsg:            "permission denied",
	}
	evidence := ph.handleLocalPermission(t.Context(), write)
	assert.Equal(t, permissionEvidenceBoundaryDenied, evidence.Kind, "the unwritable folder is the boundary")
	assert.Equal(t, issueLocalWriteDenied, evidence.IssueType)
	assert.Equal(t, "ro", evidence.BoundaryPath)
	assert.Equal(t, "ro/into-ro.bin", evidence.TriggerPath)

	read := &actionCompletion{
		Path:              "ro/secret.bin",
		ActionType:        ActionUpload,
		FailureCapability: permissionCapabilityLocalRead,
		ErrMsg:            "permission denied",
	}
	assert.Equal(t, permissionEvidenceFileDenied, ph.handleLocalPermission(t.Context(), read).Kind,
		"a file that cannot be read inside a listable folder is its own condition")
}
