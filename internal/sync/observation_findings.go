package sync

import (
	"errors"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func localObservationManagedIssueTypes() []string {
	return []string{
		IssueInvalidFilename,
		issuePathTooLong,
		issueFileTooLarge,
		issueCaseCollision,
		issueLocalReadDenied,
		issueHashPanic,
	}
}

func newLocalObservationFindingsBatch() ObservationFindingsBatch {
	return ObservationFindingsBatch{
		ManagedIssueTypes: localObservationManagedIssueTypes(),
	}
}

func newRemoteObservationFindingsBatch() ObservationFindingsBatch {
	return ObservationFindingsBatch{
		ManagedIssueTypes: []string{issueRemoteReadDenied},
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

func singlePathObservationFindingsBatch(
	driveID driveid.ID,
	managedPath string,
	observation *singlePathObservation,
) (ObservationFindingsBatch, bool) {
	if managedPath == "" {
		return ObservationFindingsBatch{}, false
	}

	batch := newLocalObservationFindingsBatch()
	appendManagedObservationPath(&batch, managedPath)

	if observation == nil || observation.Skipped == nil {
		return batch, true
	}
	if observation.Skipped.Path != "" && observation.Skipped.Path != managedPath {
		appendManagedObservationPath(&batch, observation.Skipped.Path)
	}

	appendSkippedObservationFinding(&batch, driveID, observation.Skipped)
	return batch, true
}

func appendManagedObservationPath(batch *ObservationFindingsBatch, path string) {
	if batch == nil || path == "" {
		return
	}
	for i := range batch.ManagedPaths {
		if batch.ManagedPaths[i] == path {
			return
		}
	}
	batch.ManagedPaths = append(batch.ManagedPaths, path)
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
	if item.Reason == issueLocalReadDenied && item.BlocksReadBoundary {
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
