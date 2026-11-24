package sync

import (
	"errors"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

func localObservationFindingsBatchFromSkippedItems(
	driveID driveid.ID,
	skipped []syncdomain.SkippedItem,
) syncdomain.ObservationFindingsBatch {
	batch := syncdomain.NewLocalObservationFindingsBatch()
	batch.Issues = make([]syncdomain.ObservationIssue, 0, len(skipped))

	for i := range skipped {
		appendSkippedObservationFinding(&batch, driveID, &skipped[i])
	}

	return batch
}

func appendSkippedObservationFinding(
	batch *syncdomain.ObservationFindingsBatch,
	driveID driveid.ID,
	item *syncdomain.SkippedItem,
) {
	if batch == nil || item == nil || item.Reason == "" || item.Path == "" {
		return
	}

	issue := syncdomain.ObservationIssue{
		Path:      item.Path,
		DriveID:   driveID,
		IssueType: item.Reason,
	}
	// Any finding that proved a subtree unknown (an unreadable folder, a
	// synced folder now out of reach, a mount boundary) is a read boundary:
	// the planner treats everything beneath it as unavailable, not missing.
	if item.BlocksReadBoundary {
		issue.ScopeKey = syncdomain.SKPermLocalRead(item.Path)
	}

	batch.Issues = append(batch.Issues, issue)
}

// rootReadFailureObservationFindingsBatch records that OneDrive refused the
// read of the root this engine syncs, as issueType (rootReadFailureIssueType):
// one issue at "/" under the root read scope, which makes the whole tree
// unavailable rather than absent.
func rootReadFailureObservationFindingsBatch(
	driveID driveid.ID,
	issueType syncdomain.IssueType,
) syncdomain.ObservationFindingsBatch {
	batch := newRemoteObservationFindingsBatch()
	batch.Issues = []syncdomain.ObservationIssue{{
		Path:      "/",
		DriveID:   driveID,
		IssueType: issueType,
		ScopeKey:  syncdomain.SKPermRemoteRead(""),
	}}
	return batch
}

// rootReadFailureIssueType names the issue a root observation records for
// OneDrive's answer to the read of the root: remote_root_not_found for "not
// found", remote_read_denied for "forbidden". ok is false for any other
// answer, which the observation returns as its error. The two are told apart
// only for the user: what the folder is, gone or not allowed, decides the
// next step, while sync treats both as a tree it cannot read.
func rootReadFailureIssueType(err error) (issueType syncdomain.IssueType, ok bool) {
	switch {
	case errors.Is(err, graph.ErrNotFound):
		return syncdomain.IssueRemoteRootNotFound, true
	case errors.Is(err, graph.ErrForbidden):
		return syncdomain.IssueRemoteReadDenied, true
	default:
		return "", false
	}
}

// newRemoteObservationFindingsBatch is the findings batch of one root
// observation. It manages both root read issue types, so the next observation
// replaces whichever one an earlier answer left (a "forbidden" after a "not
// found", or the reverse) and a read that succeeds clears either.
func newRemoteObservationFindingsBatch() syncdomain.ObservationFindingsBatch {
	return syncdomain.ObservationFindingsBatch{
		ManagedIssueTypes: []syncdomain.IssueType{syncdomain.IssueRemoteReadDenied, syncdomain.IssueRemoteRootNotFound},
	}
}
