package syncstore

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

const (
	computeStatusHashA = "abc123"
	computeStatusHashB = "def456"
)

type computeNewStatusCase struct {
	name          string
	currentStatus synctypes.SyncStatus
	currentHash   string
	observedHash  string
	isDeleted     bool
	filtered      bool
	wantStatus    synctypes.SyncStatus
	wantChanged   bool
}

func assertComputeNewStatusCases(t *testing.T, tests []computeNewStatusCase) {
	t.Helper()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotStatus, gotChanged := computeNewStatus(
				tt.currentStatus,
				tt.currentHash,
				tt.observedHash,
				tt.isDeleted,
				tt.filtered,
			)
			assert.Equal(t, tt.wantStatus, gotStatus, "status")
			assert.Equal(t, tt.wantChanged, gotChanged, "changed")
		})
	}
}

// TestComputeNewStatus exhaustively covers the 30-cell decision matrix from
// spec/design/data-model.md (remote_state sync_status state machine). Each cell
// maps a (currentStatus, condition) pair to an expected (newStatus, changed)
// result.

func TestComputeNewStatus_DownloadLifecycle(t *testing.T) {
	t.Parallel()

	assertComputeNewStatusCases(t, []computeNewStatusCase{
		{
			name:          "pending_download + same hash, not deleted -> no change",
			currentStatus: synctypes.SyncStatusPendingDownload,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashA,
			wantStatus:    synctypes.SyncStatusPendingDownload,
		},
		{
			name:          "pending_download + different hash, not deleted -> update hash",
			currentStatus: synctypes.SyncStatusPendingDownload,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashB,
			wantStatus:    synctypes.SyncStatusPendingDownload,
			wantChanged:   true,
		},
		{
			name:          "pending_download + deleted -> pending_delete",
			currentStatus: synctypes.SyncStatusPendingDownload,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashA,
			isDeleted:     true,
			wantStatus:    synctypes.SyncStatusPendingDelete,
			wantChanged:   true,
		},
		{
			name:          "downloading + same hash, not deleted -> no change",
			currentStatus: synctypes.SyncStatusDownloading,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashA,
			wantStatus:    synctypes.SyncStatusDownloading,
		},
		{
			name:          "downloading + different hash, not deleted -> pending_download",
			currentStatus: synctypes.SyncStatusDownloading,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashB,
			wantStatus:    synctypes.SyncStatusPendingDownload,
			wantChanged:   true,
		},
		{
			name:          "downloading + deleted -> pending_delete",
			currentStatus: synctypes.SyncStatusDownloading,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashA,
			isDeleted:     true,
			wantStatus:    synctypes.SyncStatusPendingDelete,
			wantChanged:   true,
		},
		{
			name:          "download_failed + same hash, not deleted -> pending_download",
			currentStatus: synctypes.SyncStatusDownloadFailed,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashA,
			wantStatus:    synctypes.SyncStatusPendingDownload,
			wantChanged:   true,
		},
		{
			name:          "download_failed + different hash, not deleted -> pending_download",
			currentStatus: synctypes.SyncStatusDownloadFailed,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashB,
			wantStatus:    synctypes.SyncStatusPendingDownload,
			wantChanged:   true,
		},
		{
			name:          "download_failed + deleted -> pending_delete",
			currentStatus: synctypes.SyncStatusDownloadFailed,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashA,
			isDeleted:     true,
			wantStatus:    synctypes.SyncStatusPendingDelete,
			wantChanged:   true,
		},
	})
}

func TestComputeNewStatus_SyncedAndPendingDeleteLifecycle(t *testing.T) {
	t.Parallel()

	assertComputeNewStatusCases(t, []computeNewStatusCase{
		{
			name:          "synced + same hash, not deleted -> no change",
			currentStatus: synctypes.SyncStatusSynced,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashA,
			wantStatus:    synctypes.SyncStatusSynced,
		},
		{
			name:          "synced + different hash, not deleted -> pending_download",
			currentStatus: synctypes.SyncStatusSynced,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashB,
			wantStatus:    synctypes.SyncStatusPendingDownload,
			wantChanged:   true,
		},
		{
			name:          "synced + deleted -> pending_delete",
			currentStatus: synctypes.SyncStatusSynced,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashA,
			isDeleted:     true,
			wantStatus:    synctypes.SyncStatusPendingDelete,
			wantChanged:   true,
		},
		{
			name:          "pending_delete + same hash, not deleted -> pending_download",
			currentStatus: synctypes.SyncStatusPendingDelete,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashA,
			wantStatus:    synctypes.SyncStatusPendingDownload,
			wantChanged:   true,
		},
		{
			name:          "pending_delete + different hash, not deleted -> pending_download",
			currentStatus: synctypes.SyncStatusPendingDelete,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashB,
			wantStatus:    synctypes.SyncStatusPendingDownload,
			wantChanged:   true,
		},
		{
			name:          "pending_delete + deleted -> no change",
			currentStatus: synctypes.SyncStatusPendingDelete,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashA,
			isDeleted:     true,
			wantStatus:    synctypes.SyncStatusPendingDelete,
		},
	})
}

func TestComputeNewStatus_DeleteAndFilteredLifecycle(t *testing.T) {
	t.Parallel()

	assertComputeNewStatusCases(t, []computeNewStatusCase{
		{
			name:          "deleting + same hash, not deleted -> pending_download",
			currentStatus: synctypes.SyncStatusDeleting,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashA,
			wantStatus:    synctypes.SyncStatusPendingDownload,
			wantChanged:   true,
		},
		{
			name:          "deleting + different hash, not deleted -> pending_download",
			currentStatus: synctypes.SyncStatusDeleting,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashB,
			wantStatus:    synctypes.SyncStatusPendingDownload,
			wantChanged:   true,
		},
		{
			name:          "deleting + deleted -> no change",
			currentStatus: synctypes.SyncStatusDeleting,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashA,
			isDeleted:     true,
			wantStatus:    synctypes.SyncStatusDeleting,
		},
		{
			name:          "delete_failed + same hash, not deleted -> pending_download",
			currentStatus: synctypes.SyncStatusDeleteFailed,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashA,
			wantStatus:    synctypes.SyncStatusPendingDownload,
			wantChanged:   true,
		},
		{
			name:          "delete_failed + different hash, not deleted -> pending_download",
			currentStatus: synctypes.SyncStatusDeleteFailed,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashB,
			wantStatus:    synctypes.SyncStatusPendingDownload,
			wantChanged:   true,
		},
		{
			name:          "delete_failed + deleted -> pending_delete",
			currentStatus: synctypes.SyncStatusDeleteFailed,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashA,
			isDeleted:     true,
			wantStatus:    synctypes.SyncStatusPendingDelete,
			wantChanged:   true,
		},
		{
			name:          "deleted + same hash, not deleted -> pending_download",
			currentStatus: synctypes.SyncStatusDeleted,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashA,
			wantStatus:    synctypes.SyncStatusPendingDownload,
			wantChanged:   true,
		},
		{
			name:          "deleted + different hash, not deleted -> pending_download",
			currentStatus: synctypes.SyncStatusDeleted,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashB,
			wantStatus:    synctypes.SyncStatusPendingDownload,
			wantChanged:   true,
		},
		{
			name:          "deleted + deleted -> no change",
			currentStatus: synctypes.SyncStatusDeleted,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashA,
			isDeleted:     true,
			wantStatus:    synctypes.SyncStatusDeleted,
		},
	})
}

func TestComputeNewStatus_FilteredLifecycle(t *testing.T) {
	t.Parallel()

	assertComputeNewStatusCases(t, []computeNewStatusCase{
		{
			name:          "filtered + same hash, not deleted -> no change",
			currentStatus: synctypes.SyncStatusFiltered,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashA,
			filtered:      true,
			wantStatus:    synctypes.SyncStatusFiltered,
		},
		{
			name:          "filtered + different hash, not deleted -> update hash",
			currentStatus: synctypes.SyncStatusFiltered,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashB,
			filtered:      true,
			wantStatus:    synctypes.SyncStatusFiltered,
			wantChanged:   true,
		},
		{
			name:          "filtered + deleted -> deleted",
			currentStatus: synctypes.SyncStatusFiltered,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashA,
			isDeleted:     true,
			wantStatus:    synctypes.SyncStatusDeleted,
			wantChanged:   true,
		},
		{
			name:          "filtered row re-entering with same hash -> synced",
			currentStatus: synctypes.SyncStatusFiltered,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashA,
			wantStatus:    synctypes.SyncStatusSynced,
			wantChanged:   true,
		},
		{
			name:          "filtered row re-entering with different hash -> pending_download",
			currentStatus: synctypes.SyncStatusFiltered,
			currentHash:   computeStatusHashA,
			observedHash:  computeStatusHashB,
			wantStatus:    synctypes.SyncStatusPendingDownload,
			wantChanged:   true,
		},
	})
}

func TestComputeNewStatus_EmptyHashCases(t *testing.T) {
	t.Parallel()

	assertComputeNewStatusCases(t, []computeNewStatusCase{
		{
			name:          "synced + both empty hash -> no change",
			currentStatus: synctypes.SyncStatusSynced,
			wantStatus:    synctypes.SyncStatusSynced,
		},
		{
			name:          "synced + empty current, non-empty observed -> pending_download",
			currentStatus: synctypes.SyncStatusSynced,
			observedHash:  computeStatusHashA,
			wantStatus:    synctypes.SyncStatusPendingDownload,
			wantChanged:   true,
		},
		{
			name:          "synced + non-empty current, empty observed -> pending_download",
			currentStatus: synctypes.SyncStatusSynced,
			currentHash:   computeStatusHashA,
			wantStatus:    synctypes.SyncStatusPendingDownload,
			wantChanged:   true,
		},
	})
}
