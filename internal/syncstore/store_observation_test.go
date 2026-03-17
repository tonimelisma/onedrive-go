package syncstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
		currentStatus string
		currentHash   string
		observedHash  string
		isDeleted     bool
		wantStatus    string
		wantChanged   bool
	}{
		// --- pending_download ---
		{
			name:          "pending_download + same hash, not deleted → no change",
			currentStatus: "pending_download", currentHash: hashA,
			observedHash: hashA, isDeleted: false,
			wantStatus: "pending_download", wantChanged: false,
		},
		{
			name:          "pending_download + different hash, not deleted → update hash",
			currentStatus: "pending_download", currentHash: hashA,
			observedHash: hashB, isDeleted: false,
			wantStatus: "pending_download", wantChanged: true,
		},
		{
			name:          "pending_download + deleted → pending_delete",
			currentStatus: "pending_download", currentHash: hashA,
			observedHash: hashA, isDeleted: true,
			wantStatus: "pending_delete", wantChanged: true,
		},

		// --- downloading ---
		{
			name:          "downloading + same hash, not deleted → no change (let worker finish)",
			currentStatus: "downloading", currentHash: hashA,
			observedHash: hashA, isDeleted: false,
			wantStatus: "downloading", wantChanged: false,
		},
		{
			name:          "downloading + different hash, not deleted → pending_download + cancel",
			currentStatus: "downloading", currentHash: hashA,
			observedHash: hashB, isDeleted: false,
			wantStatus: "pending_download", wantChanged: true,
		},
		{
			name:          "downloading + deleted → pending_delete + cancel",
			currentStatus: "downloading", currentHash: hashA,
			observedHash: hashA, isDeleted: true,
			wantStatus: "pending_delete", wantChanged: true,
		},

		// --- download_failed ---
		{
			name:          "download_failed + same hash, not deleted → pending_download (retry)",
			currentStatus: "download_failed", currentHash: hashA,
			observedHash: hashA, isDeleted: false,
			wantStatus: "pending_download", wantChanged: true,
		},
		{
			name:          "download_failed + different hash, not deleted → pending_download (new version)",
			currentStatus: "download_failed", currentHash: hashA,
			observedHash: hashB, isDeleted: false,
			wantStatus: "pending_download", wantChanged: true,
		},
		{
			name:          "download_failed + deleted → pending_delete",
			currentStatus: "download_failed", currentHash: hashA,
			observedHash: hashA, isDeleted: true,
			wantStatus: "pending_delete", wantChanged: true,
		},

		// --- synced (critical row) ---
		{
			name:          "synced + same hash, not deleted → NO CHANGE (prevents re-download on delta redelivery)",
			currentStatus: "synced", currentHash: hashA,
			observedHash: hashA, isDeleted: false,
			wantStatus: "synced", wantChanged: false,
		},
		{
			name:          "synced + different hash, not deleted → pending_download",
			currentStatus: "synced", currentHash: hashA,
			observedHash: hashB, isDeleted: false,
			wantStatus: "pending_download", wantChanged: true,
		},
		{
			name:          "synced + deleted → pending_delete",
			currentStatus: "synced", currentHash: hashA,
			observedHash: hashA, isDeleted: true,
			wantStatus: "pending_delete", wantChanged: true,
		},

		// --- pending_delete ---
		{
			name:          "pending_delete + same hash, not deleted → pending_download (restored)",
			currentStatus: "pending_delete", currentHash: hashA,
			observedHash: hashA, isDeleted: false,
			wantStatus: "pending_download", wantChanged: true,
		},
		{
			name:          "pending_delete + different hash, not deleted → pending_download (restored+changed)",
			currentStatus: "pending_delete", currentHash: hashA,
			observedHash: hashB, isDeleted: false,
			wantStatus: "pending_download", wantChanged: true,
		},
		{
			name:          "pending_delete + deleted → no change",
			currentStatus: "pending_delete", currentHash: hashA,
			observedHash: hashA, isDeleted: true,
			wantStatus: "pending_delete", wantChanged: false,
		},

		// --- deleting ---
		{
			name:          "deleting + same hash, not deleted → pending_download (restored)",
			currentStatus: "deleting", currentHash: hashA,
			observedHash: hashA, isDeleted: false,
			wantStatus: "pending_download", wantChanged: true,
		},
		{
			name:          "deleting + different hash, not deleted → pending_download (restored)",
			currentStatus: "deleting", currentHash: hashA,
			observedHash: hashB, isDeleted: false,
			wantStatus: "pending_download", wantChanged: true,
		},
		{
			name:          "deleting + deleted → no change (let worker finish)",
			currentStatus: "deleting", currentHash: hashA,
			observedHash: hashA, isDeleted: true,
			wantStatus: "deleting", wantChanged: false,
		},

		// --- delete_failed ---
		{
			name:          "delete_failed + same hash, not deleted → pending_download (restored)",
			currentStatus: "delete_failed", currentHash: hashA,
			observedHash: hashA, isDeleted: false,
			wantStatus: "pending_download", wantChanged: true,
		},
		{
			name:          "delete_failed + different hash, not deleted → pending_download (restored)",
			currentStatus: "delete_failed", currentHash: hashA,
			observedHash: hashB, isDeleted: false,
			wantStatus: "pending_download", wantChanged: true,
		},
		{
			name:          "delete_failed + deleted → pending_delete (retry)",
			currentStatus: "delete_failed", currentHash: hashA,
			observedHash: hashA, isDeleted: true,
			wantStatus: "pending_delete", wantChanged: true,
		},

		// --- deleted ---
		{
			name:          "deleted + same hash, not deleted → pending_download (recreated)",
			currentStatus: "deleted", currentHash: hashA,
			observedHash: hashA, isDeleted: false,
			wantStatus: "pending_download", wantChanged: true,
		},
		{
			name:          "deleted + different hash, not deleted → pending_download (recreated)",
			currentStatus: "deleted", currentHash: hashA,
			observedHash: hashB, isDeleted: false,
			wantStatus: "pending_download", wantChanged: true,
		},
		{
			name:          "deleted + deleted → no change",
			currentStatus: "deleted", currentHash: hashA,
			observedHash: hashA, isDeleted: true,
			wantStatus: "deleted", wantChanged: false,
		},

		// --- filtered ---
		{
			name:          "filtered + same hash, not deleted → no change",
			currentStatus: "filtered", currentHash: hashA,
			observedHash: hashA, isDeleted: false,
			wantStatus: "filtered", wantChanged: false,
		},
		{
			name:          "filtered + different hash, not deleted → update hash (stay filtered)",
			currentStatus: "filtered", currentHash: hashA,
			observedHash: hashB, isDeleted: false,
			wantStatus: "filtered", wantChanged: true,
		},
		{
			name:          "filtered + deleted → deleted",
			currentStatus: "filtered", currentHash: hashA,
			observedHash: hashA, isDeleted: true,
			wantStatus: "deleted", wantChanged: true,
		},

		// --- Edge cases: empty hash handling ---
		{
			name:          "synced + both empty hash → no change (zero-byte file redelivery)",
			currentStatus: "synced", currentHash: "",
			observedHash: "", isDeleted: false,
			wantStatus: "synced", wantChanged: false,
		},
		{
			name:          "synced + empty current, non-empty observed → pending_download",
			currentStatus: "synced", currentHash: "",
			observedHash: hashA, isDeleted: false,
			wantStatus: "pending_download", wantChanged: true,
		},
		{
			name:          "synced + non-empty current, empty observed → pending_download",
			currentStatus: "synced", currentHash: hashA,
			observedHash: "", isDeleted: false,
			wantStatus: "pending_download", wantChanged: true,
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
