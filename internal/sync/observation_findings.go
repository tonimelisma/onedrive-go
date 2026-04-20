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

func newLocalObservationFindingsBatch() ObservationFindingsBatch {
	return ObservationFindingsBatch{
		ManagedIssueTypes:     localObservationManagedIssueTypes(),
		ManagedReadScopeKinds: localObservationManagedReadScopeKinds(),
	}
}

func newRemoteObservationFindingsBatch() ObservationFindingsBatch {
	return ObservationFindingsBatch{
		ManagedIssueTypes:     []string{IssueRemoteReadDenied},
		ManagedReadScopeKinds: []ScopeKeyKind{ScopePermRemoteRead},
	}
}

func localObservationFindingsBatchFromSkippedItems(
	driveID driveid.ID,
	skipped []SkippedItem,
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
	observation *SinglePathObservation,
) (ObservationFindingsBatch, bool) {
	if managedPath == "" {
		return ObservationFindingsBatch{}, false
	}

	batch := newLocalObservationFindingsBatch()
	appendManagedObservationPath(&batch, managedPath)
	appendManagedObservationReadScope(&batch, SKPermLocalRead(managedPath))

	if observation == nil || observation.Skipped == nil {
		return batch, true
	}
	if observation.Skipped.Path != "" && observation.Skipped.Path != managedPath {
		appendManagedObservationPath(&batch, observation.Skipped.Path)
		appendManagedObservationReadScope(&batch, SKPermLocalRead(observation.Skipped.Path))
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

func appendManagedObservationReadScope(batch *ObservationFindingsBatch, key ScopeKey) {
	if batch == nil || key.IsZero() {
		return
	}
	for i := range batch.ManagedReadScopes {
		if batch.ManagedReadScopes[i] == key {
			return
		}
	}
	batch.ManagedReadScopes = append(batch.ManagedReadScopes, key)
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

func rootRemoteReadDeniedObservationFindingsBatch(
	driveID driveid.ID,
	err error,
) ObservationFindingsBatch {
	return remoteReadDeniedObservationBatch(driveID, "/", SKPermRemoteRead(""), err)
}

func remoteReadDeniedObservationBatch(
	driveID driveid.ID,
	path string,
	scopeKey ScopeKey,
	err error,
) ObservationFindingsBatch {
	batch := newRemoteObservationFindingsBatch()
	batch.Issues = []ObservationIssue{{
		Path:       path,
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
