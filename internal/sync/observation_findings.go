package sync

import (
	"errors"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func remoteObservationManagedBatch() ObservationFindingsBatch {
	return ObservationFindingsBatch{
		ManagedIssueTypes:     []string{IssueRemoteReadDenied},
		ManagedReadScopeKinds: []ScopeKeyKind{ScopePermRemoteRead},
	}
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
