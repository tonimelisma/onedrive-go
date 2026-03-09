package sync

// Remote state sync_status values. These match the CHECK constraint in
// the remote_state table (migrations/00001_consolidated_schema.sql).
const (
	statusPendingDownload = "pending_download"
	statusDownloading     = "downloading"
	statusDownloadFailed  = "download_failed"
	statusSynced          = "synced"
	statusPendingDelete   = "pending_delete"
	statusDeleting        = "deleting"
	statusDeleteFailed    = "delete_failed"
	statusDeleted         = "deleted"
	statusFiltered        = "filtered"
)

// computeNewStatus determines the new sync_status for a remote_state row
// when a delta observation arrives. Pure function — no I/O, no side effects.
//
// Returns (newStatus, changed). changed=false means the row should not be
// updated (the observation is a no-op for this row).
//
// Implements the 30-cell decision matrix from
// spec/design/data-model.md (remote_state sync_status state machine).
func computeNewStatus(currentStatus, currentHash, observedHash string, isDeleted bool) (string, bool) {
	sameHash := currentHash == observedHash

	if isDeleted {
		return computeDeleted(currentStatus)
	}

	if sameHash {
		return computeSameHash(currentStatus)
	}

	return computeDifferentHash(currentStatus)
}

// computeDeleted handles the "deleted" column of the decision matrix.
func computeDeleted(currentStatus string) (string, bool) {
	switch currentStatus {
	case statusPendingDownload, statusDownloading, statusDownloadFailed, statusSynced:
		return statusPendingDelete, true
	case statusPendingDelete:
		return statusPendingDelete, false // already pending delete
	case statusDeleting:
		return statusDeleting, false // let worker finish
	case statusDeleteFailed:
		return statusPendingDelete, true // retry
	case statusDeleted:
		return statusDeleted, false // already deleted
	case statusFiltered:
		return statusDeleted, true
	default:
		return currentStatus, false
	}
}

// computeSameHash handles the "same hash, not deleted" column.
func computeSameHash(currentStatus string) (string, bool) {
	switch currentStatus {
	case statusPendingDownload, statusDownloading:
		return currentStatus, false // no change / let worker finish
	case statusDownloadFailed:
		return statusPendingDownload, true // retry
	case statusSynced:
		// Critical: prevents re-download on delta redelivery.
		return statusSynced, false
	case statusPendingDelete, statusDeleting, statusDeleteFailed, statusDeleted:
		return statusPendingDownload, true // restored/recreated
	case statusFiltered:
		return statusFiltered, false
	default:
		return currentStatus, false
	}
}

// computeDifferentHash handles the "different hash, not deleted" column.
func computeDifferentHash(currentStatus string) (string, bool) {
	switch currentStatus {
	case statusPendingDownload:
		return statusPendingDownload, true // update hash (still pending)
	case statusDownloading:
		return statusPendingDownload, true // cancel + re-queue
	case statusDownloadFailed:
		return statusPendingDownload, true // new version
	case statusSynced:
		return statusPendingDownload, true
	case statusPendingDelete, statusDeleting, statusDeleteFailed, statusDeleted:
		return statusPendingDownload, true // restored+changed / recreated
	case statusFiltered:
		return statusFiltered, true // update hash, stay filtered
	default:
		return currentStatus, false
	}
}
