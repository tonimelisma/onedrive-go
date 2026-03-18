package syncstore

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// TestComputeNewStatus exhaustively covers the 30-cell decision matrix from
// spec/design/data-model.md (remote_state sync_status state machine). Each cell maps a (currentStatus,
// condition) pair to an expected (newStatus, changed) result.
func TestComputeNewStatus(t *testing.T) {
	t.Parallel()

	const (
		hashA = "abc123"
		hashB = "def456"
	)

	tests := []struct {
		name          string
		currentStatus synctypes.SyncStatus
		currentHash   string
		observedHash  string
		isDeleted     bool
		wantStatus    synctypes.SyncStatus
		wantChanged   bool
	}{
		// --- pending_download ---
		{
			name:          "pending_download + same hash, not deleted → no change",
			currentStatus: synctypes.SyncStatusPendingDownload, currentHash: hashA,
			observedHash: hashA, isDeleted: false,
			wantStatus: synctypes.SyncStatusPendingDownload, wantChanged: false,
		},
		{
			name:          "pending_download + different hash, not deleted → update hash",
			currentStatus: synctypes.SyncStatusPendingDownload, currentHash: hashA,
			observedHash: hashB, isDeleted: false,
			wantStatus: synctypes.SyncStatusPendingDownload, wantChanged: true,
		},
		{
			name:          "pending_download + deleted → pending_delete",
			currentStatus: synctypes.SyncStatusPendingDownload, currentHash: hashA,
			observedHash: hashA, isDeleted: true,
			wantStatus: synctypes.SyncStatusPendingDelete, wantChanged: true,
		},

		// --- downloading ---
		{
			name:          "downloading + same hash, not deleted → no change (let worker finish)",
			currentStatus: synctypes.SyncStatusDownloading, currentHash: hashA,
			observedHash: hashA, isDeleted: false,
			wantStatus: synctypes.SyncStatusDownloading, wantChanged: false,
		},
		{
			name:          "downloading + different hash, not deleted → pending_download + cancel",
			currentStatus: synctypes.SyncStatusDownloading, currentHash: hashA,
			observedHash: hashB, isDeleted: false,
			wantStatus: synctypes.SyncStatusPendingDownload, wantChanged: true,
		},
		{
			name:          "downloading + deleted → pending_delete + cancel",
			currentStatus: synctypes.SyncStatusDownloading, currentHash: hashA,
			observedHash: hashA, isDeleted: true,
			wantStatus: synctypes.SyncStatusPendingDelete, wantChanged: true,
		},

		// --- download_failed ---
		{
			name:          "download_failed + same hash, not deleted → pending_download (retry)",
			currentStatus: synctypes.SyncStatusDownloadFailed, currentHash: hashA,
			observedHash: hashA, isDeleted: false,
			wantStatus: synctypes.SyncStatusPendingDownload, wantChanged: true,
		},
		{
			name:          "download_failed + different hash, not deleted → pending_download (new version)",
			currentStatus: synctypes.SyncStatusDownloadFailed, currentHash: hashA,
			observedHash: hashB, isDeleted: false,
			wantStatus: synctypes.SyncStatusPendingDownload, wantChanged: true,
		},
		{
			name:          "download_failed + deleted → pending_delete",
			currentStatus: synctypes.SyncStatusDownloadFailed, currentHash: hashA,
			observedHash: hashA, isDeleted: true,
			wantStatus: synctypes.SyncStatusPendingDelete, wantChanged: true,
		},

		// --- synced (critical row) ---
		{
			name:          "synced + same hash, not deleted → NO CHANGE (prevents re-download on delta redelivery)",
			currentStatus: synctypes.SyncStatusSynced, currentHash: hashA,
			observedHash: hashA, isDeleted: false,
			wantStatus: synctypes.SyncStatusSynced, wantChanged: false,
		},
		{
			name:          "synced + different hash, not deleted → pending_download",
			currentStatus: synctypes.SyncStatusSynced, currentHash: hashA,
			observedHash: hashB, isDeleted: false,
			wantStatus: synctypes.SyncStatusPendingDownload, wantChanged: true,
		},
		{
			name:          "synced + deleted → pending_delete",
			currentStatus: synctypes.SyncStatusSynced, currentHash: hashA,
			observedHash: hashA, isDeleted: true,
			wantStatus: synctypes.SyncStatusPendingDelete, wantChanged: true,
		},

		// --- pending_delete ---
		{
			name:          "pending_delete + same hash, not deleted → pending_download (restored)",
			currentStatus: synctypes.SyncStatusPendingDelete, currentHash: hashA,
			observedHash: hashA, isDeleted: false,
			wantStatus: synctypes.SyncStatusPendingDownload, wantChanged: true,
		},
		{
			name:          "pending_delete + different hash, not deleted → pending_download (restored+changed)",
			currentStatus: synctypes.SyncStatusPendingDelete, currentHash: hashA,
			observedHash: hashB, isDeleted: false,
			wantStatus: synctypes.SyncStatusPendingDownload, wantChanged: true,
		},
		{
			name:          "pending_delete + deleted → no change",
			currentStatus: synctypes.SyncStatusPendingDelete, currentHash: hashA,
			observedHash: hashA, isDeleted: true,
			wantStatus: synctypes.SyncStatusPendingDelete, wantChanged: false,
		},

		// --- deleting ---
		{
			name:          "deleting + same hash, not deleted → pending_download (restored)",
			currentStatus: synctypes.SyncStatusDeleting, currentHash: hashA,
			observedHash: hashA, isDeleted: false,
			wantStatus: synctypes.SyncStatusPendingDownload, wantChanged: true,
		},
		{
			name:          "deleting + different hash, not deleted → pending_download (restored)",
			currentStatus: synctypes.SyncStatusDeleting, currentHash: hashA,
			observedHash: hashB, isDeleted: false,
			wantStatus: synctypes.SyncStatusPendingDownload, wantChanged: true,
		},
		{
			name:          "deleting + deleted → no change (let worker finish)",
			currentStatus: synctypes.SyncStatusDeleting, currentHash: hashA,
			observedHash: hashA, isDeleted: true,
			wantStatus: synctypes.SyncStatusDeleting, wantChanged: false,
		},

		// --- delete_failed ---
		{
			name:          "delete_failed + same hash, not deleted → pending_download (restored)",
			currentStatus: synctypes.SyncStatusDeleteFailed, currentHash: hashA,
			observedHash: hashA, isDeleted: false,
			wantStatus: synctypes.SyncStatusPendingDownload, wantChanged: true,
		},
		{
			name:          "delete_failed + different hash, not deleted → pending_download (restored)",
			currentStatus: synctypes.SyncStatusDeleteFailed, currentHash: hashA,
			observedHash: hashB, isDeleted: false,
			wantStatus: synctypes.SyncStatusPendingDownload, wantChanged: true,
		},
		{
			name:          "delete_failed + deleted → pending_delete (retry)",
			currentStatus: synctypes.SyncStatusDeleteFailed, currentHash: hashA,
			observedHash: hashA, isDeleted: true,
			wantStatus: synctypes.SyncStatusPendingDelete, wantChanged: true,
		},

		// --- deleted ---
		{
			name:          "deleted + same hash, not deleted → pending_download (recreated)",
			currentStatus: synctypes.SyncStatusDeleted, currentHash: hashA,
			observedHash: hashA, isDeleted: false,
			wantStatus: synctypes.SyncStatusPendingDownload, wantChanged: true,
		},
		{
			name:          "deleted + different hash, not deleted → pending_download (recreated)",
			currentStatus: synctypes.SyncStatusDeleted, currentHash: hashA,
			observedHash: hashB, isDeleted: false,
			wantStatus: synctypes.SyncStatusPendingDownload, wantChanged: true,
		},
		{
			name:          "deleted + deleted → no change",
			currentStatus: synctypes.SyncStatusDeleted, currentHash: hashA,
			observedHash: hashA, isDeleted: true,
			wantStatus: synctypes.SyncStatusDeleted, wantChanged: false,
		},

		// --- filtered ---
		{
			name:          "filtered + same hash, not deleted → no change",
			currentStatus: synctypes.SyncStatusFiltered, currentHash: hashA,
			observedHash: hashA, isDeleted: false,
			wantStatus: synctypes.SyncStatusFiltered, wantChanged: false,
		},
		{
			name:          "filtered + different hash, not deleted → update hash (stay filtered)",
			currentStatus: synctypes.SyncStatusFiltered, currentHash: hashA,
			observedHash: hashB, isDeleted: false,
			wantStatus: synctypes.SyncStatusFiltered, wantChanged: true,
		},
		{
			name:          "filtered + deleted → deleted",
			currentStatus: synctypes.SyncStatusFiltered, currentHash: hashA,
			observedHash: hashA, isDeleted: true,
			wantStatus: synctypes.SyncStatusDeleted, wantChanged: true,
		},

		// --- Edge cases: empty hash handling ---
		{
			name:          "synced + both empty hash → no change (zero-byte file redelivery)",
			currentStatus: synctypes.SyncStatusSynced, currentHash: "",
			observedHash: "", isDeleted: false,
			wantStatus: synctypes.SyncStatusSynced, wantChanged: false,
		},
		{
			name:          "synced + empty current, non-empty observed → pending_download",
			currentStatus: synctypes.SyncStatusSynced, currentHash: "",
			observedHash: hashA, isDeleted: false,
			wantStatus: synctypes.SyncStatusPendingDownload, wantChanged: true,
		},
		{
			name:          "synced + non-empty current, empty observed → pending_download",
			currentStatus: synctypes.SyncStatusSynced, currentHash: hashA,
			observedHash: "", isDeleted: false,
			wantStatus: synctypes.SyncStatusPendingDownload, wantChanged: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotStatus, gotChanged := computeNewStatus(tt.currentStatus, tt.currentHash, tt.observedHash, tt.isDeleted)
			assert.Equal(t, tt.wantStatus, gotStatus, "status")
			assert.Equal(t, tt.wantChanged, gotChanged, "changed")
		})
	}
}
