package sync

import (
	"errors"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// localObservationManagedIssueTypes lists the issue types a full local scan
// owns: its findings batch replaces the stored conditions of exactly these
// types, so a condition the scan does not report again is cleared. Every
// type a local scan reports is listed, because every full scan re-reports a
// finding whose cause is still there; a type left out keeps its condition
// after its cause is gone. Leaving one out on purpose needs its reason
// recorded where TestLocalObservationManagedIssueTypes_ManageEveryTypeALocalScanReports
// checks this list against the types the scanner's source names.
func localObservationManagedIssueTypes() []IssueType {
	return []IssueType{
		IssueInvalidFilename,
		issuePathTooLong,
		issueFileTooLarge,
		issueCaseCollision,
		issueLocalReadDenied,
		issueHashPanic,
		// A boundary the scan proved last time (a synced path now holding a
		// symlink or FIFO, a mount point of another filesystem) is managed
		// too: when a later full scan finds the path representable again, the
		// condition clears and the path syncs again. A crossed mount point
		// that is not mounted is re-reported by every scan, so it stays
		// (R-6.2.14, R-6.2.15).
		issueLocalUnsyncableReplacement,
		issueLocalMountBoundary,
		// The leftover of an interrupted removal is reported by every scan
		// that finds it and by nothing else, so the scan that no longer finds
		// it (the user renamed it back or deleted it) clears the report. The
		// file itself is never touched (R-6.4.9).
		issueLocalInterruptedRemoval,
	}
}

func newLocalObservationFindingsBatch() ObservationFindingsBatch {
	return ObservationFindingsBatch{
		ManagedIssueTypes: localObservationManagedIssueTypes(),
	}
}

func newRemoteObservationFindingsBatch() ObservationFindingsBatch {
	return ObservationFindingsBatch{
		ManagedIssueTypes: []IssueType{issueRemoteReadDenied},
	}
}

func localObservationFindingsBatchFromSkippedItems(
	driveID driveid.ID,
	skipped []skippedItem,
) ObservationFindingsBatch {
	batch := newLocalObservationFindingsBatch()
	batch.Issues = make([]ObservationIssue, 0, len(skipped))

	for i := range skipped {
		appendSkippedObservationFinding(&batch, driveID, &skipped[i])
	}

	return batch
}

func appendSkippedObservationFinding(
	batch *ObservationFindingsBatch,
	driveID driveid.ID,
	item *skippedItem,
) {
	if batch == nil || item == nil || item.Reason == "" || item.Path == "" {
		return
	}

	issue := ObservationIssue{
		Path:      item.Path,
		DriveID:   driveID,
		IssueType: item.Reason,
	}
	// Any finding that proved a subtree unknown (an unreadable folder, a
	// synced folder now out of reach, a mount boundary) is a read boundary:
	// the planner treats everything beneath it as unavailable, not missing.
	if item.BlocksReadBoundary {
		issue.ScopeKey = sKPermLocalRead(item.Path)
	}

	batch.Issues = append(batch.Issues, issue)
}

func rootRemoteReadDeniedObservationFindingsBatch(
	driveID driveid.ID,
) ObservationFindingsBatch {
	return remoteReadDeniedObservationBatch(driveID, "/", sKPermRemoteRead(""))
}

func remoteReadDeniedObservationBatch(
	driveID driveid.ID,
	path string,
	scopeKey ScopeKey,
) ObservationFindingsBatch {
	batch := newRemoteObservationFindingsBatch()
	batch.Issues = []ObservationIssue{{
		Path:      path,
		DriveID:   driveID,
		IssueType: issueRemoteReadDenied,
		ScopeKey:  scopeKey,
	}}
	return batch
}

func isObservationRemoteReadDenied(err error) bool {
	return errors.Is(err, graph.ErrForbidden) || errors.Is(err, graph.ErrNotFound)
}
