package sync

import (
	"errors"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func localObservationManagedIssueTypes() []string {
	return []string{
		IssueInvalidFilename,
		IssuePathTooLong,
		IssueFileTooLarge,
		IssueCaseCollision,
		IssueLocalReadDenied,
		IssueHashPanic,
	}
}

func localObservationManagedReadScopeKinds() []ScopeKeyKind {
	return []ScopeKeyKind{ScopePermDirRead}
}

func localObservationManagedBatch() ObservationFindingsBatch {
	return ObservationFindingsBatch{
		ManagedIssueTypes:     localObservationManagedIssueTypes(),
		ManagedReadScopeKinds: localObservationManagedReadScopeKinds(),
	}
}

func remoteObservationManagedBatch() ObservationFindingsBatch {
	return ObservationFindingsBatch{
		ManagedIssueTypes:     []string{IssueRemoteReadDenied},
		ManagedReadScopeKinds: []ScopeKeyKind{ScopePermRemoteRead},
	}
}

func observationFindingsBatchFromSkippedItems(
	driveID driveid.ID,
	skipped []SkippedItem,
) ObservationFindingsBatch {
	batch := localObservationManagedBatch()
	batch.Issues = make([]ObservationIssue, 0, len(skipped))

	for i := range skipped {
		appendSkippedObservationFinding(&batch, driveID, &skipped[i])
	}

	return batch
}

func observationFindingsBatchFromSinglePathObservation(
	driveID driveid.ID,
	managedPath string,
	observation *SinglePathObservation,
) (ObservationFindingsBatch, bool) {
	if managedPath == "" {
		return ObservationFindingsBatch{}, false
	}

	batch := localObservationManagedBatch()
	batch.ManagedPaths = []string{managedPath}
	batch.ManagedReadScopes = []ScopeKey{SKPermLocalRead(managedPath)}

	if observation == nil || observation.Skipped == nil {
		return batch, true
	}
	if observation.Skipped.Path != "" && observation.Skipped.Path != managedPath {
		batch.ManagedPaths = append(batch.ManagedPaths, observation.Skipped.Path)
		batch.ManagedReadScopes = append(batch.ManagedReadScopes, SKPermLocalRead(observation.Skipped.Path))
	}

	appendSkippedObservationFinding(&batch, driveID, observation.Skipped)
	return batch, true
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
		Path:       item.Path,
		DriveID:    driveID,
		ActionType: ActionUpload,
		IssueType:  item.Reason,
		Error:      item.Detail,
		FileSize:   item.FileSize,
	}
	if item.Reason == IssueLocalReadDenied && item.BlocksReadScope {
		issue.ScopeKey = SKPermLocalRead(item.Path)
		batch.ReadScopes = append(batch.ReadScopes, issue.ScopeKey)
	}

	batch.Issues = append(batch.Issues, issue)
}

func rootRemoteReadDeniedObservationBatch(
	driveID driveid.ID,
	err error,
) ObservationFindingsBatch {
	batch := remoteObservationManagedBatch()
	scopeKey := SKPermRemoteRead("")
	batch.Issues = []ObservationIssue{{
		Path:       "/",
		DriveID:    driveID,
		ActionType: ActionDownload,
		IssueType:  IssueRemoteReadDenied,
		Error:      err.Error(),
		ScopeKey:   scopeKey,
	}}
	batch.ReadScopes = []ScopeKey{scopeKey}
	return batch
}

func isObservationRemoteReadDenied(err error) bool {
	return errors.Is(err, graph.ErrForbidden) || errors.Is(err, graph.ErrNotFound)
}
