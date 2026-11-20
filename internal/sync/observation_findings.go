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
		IssuePathTooLong,
		IssueFileTooLarge,
		IssueCaseCollision,
		IssueLocalReadDenied,
		IssueHashPanic,
		// A boundary the scan proved last time (a synced path now holding a
		// symlink or FIFO, a mount point of another filesystem) is managed
		// too: when a later full scan finds the path representable again, the
		// condition clears and the path syncs again. A crossed mount point
		// that is not mounted is re-reported by every scan, so it stays until
		// the disk is back (R-6.2.14, R-6.2.15). Both mount types are managed
		// together, so a scan that reports one at a path replaces the other.
		IssueLocalUnsyncableReplacement,
		IssueLocalMountBoundary,
		IssueLocalMountMissing,
		// The leftover of an interrupted removal is reported by every scan
		// that finds it and by nothing else, so the scan that no longer finds
		// it (the user renamed it back or deleted it) clears the report. The
		// file itself is never touched (R-6.4.9).
		IssueLocalInterruptedRemoval,
		// What a removed shared folder's release kept is recorded by the
		// release after the run's last plan and by nothing else, so every
		// full scan clears it: a release that runs again records what is
		// still there, and a folder shared again keeps no stale report
		// (R-2.4.8).
		IssueRemovedFolderKept,
	}
}

func NewLocalObservationFindingsBatch() ObservationFindingsBatch {
	return ObservationFindingsBatch{
		ManagedIssueTypes: localObservationManagedIssueTypes(),
	}
}

// NewRemoteObservationFindingsBatch is the findings batch of one root
// observation. It manages both root read issue types, so the next observation
// replaces whichever one an earlier answer left (a "forbidden" after a "not
// found", or the reverse) and a read that succeeds clears either.
func NewRemoteObservationFindingsBatch() ObservationFindingsBatch {
	return ObservationFindingsBatch{
		ManagedIssueTypes: []IssueType{IssueRemoteReadDenied, IssueRemoteRootNotFound},
	}
}

func localObservationFindingsBatchFromSkippedItems(
	driveID driveid.ID,
	skipped []SkippedItem,
) ObservationFindingsBatch {
	batch := NewLocalObservationFindingsBatch()
	batch.Issues = make([]ObservationIssue, 0, len(skipped))

	for i := range skipped {
		appendSkippedObservationFinding(&batch, driveID, &skipped[i])
	}

	return batch
}

func appendSkippedObservationFinding(
	batch *ObservationFindingsBatch,
	driveID driveid.ID,
	item *SkippedItem,
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
		issue.ScopeKey = SKPermLocalRead(item.Path)
	}

	batch.Issues = append(batch.Issues, issue)
}

// rootReadFailureObservationFindingsBatch records that OneDrive refused the
// read of the root this engine syncs, as issueType (rootReadFailureIssueType):
// one issue at "/" under the root read scope, which makes the whole tree
// unavailable rather than absent.
func rootReadFailureObservationFindingsBatch(
	driveID driveid.ID,
	issueType IssueType,
) ObservationFindingsBatch {
	batch := NewRemoteObservationFindingsBatch()
	batch.Issues = []ObservationIssue{{
		Path:      "/",
		DriveID:   driveID,
		IssueType: issueType,
		ScopeKey:  SKPermRemoteRead(""),
	}}
	return batch
}

// rootReadFailureIssueType names the issue a root observation records for
// OneDrive's answer to the read of the root: remote_root_not_found for "not
// found", remote_read_denied for "forbidden". ok is false for any other
// answer, which the observation returns as its error. The two are told apart
// only for the user: what the folder is, gone or not allowed, decides the
// next step, while sync treats both as a tree it cannot read.
func rootReadFailureIssueType(err error) (issueType IssueType, ok bool) {
	switch {
	case errors.Is(err, graph.ErrNotFound):
		return IssueRemoteRootNotFound, true
	case errors.Is(err, graph.ErrForbidden):
		return IssueRemoteReadDenied, true
	default:
		return "", false
	}
}
