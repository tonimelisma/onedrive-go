package sync

import (
	"context"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type observationWriter interface {
	CommitObservation(ctx context.Context, events []syncstore.ObservedItem, newToken string, driveID driveid.ID) error
}

type failureRecorder interface {
	RecordFailure(ctx context.Context, p *syncstore.SyncFailureParams, delayFn func(int) time.Duration) error
	ListActionableFailures(ctx context.Context) ([]syncstore.SyncFailureRow, error)
	ListSyncFailuresByIssueType(ctx context.Context, issueType string) ([]syncstore.SyncFailureRow, error)
	ListRemoteBlockedFailures(ctx context.Context) ([]syncstore.SyncFailureRow, error)
	ClearSyncFailure(ctx context.Context, path string, driveID driveid.ID) error
	ClearActionableSyncFailures(ctx context.Context) error
	MarkSyncFailureActionable(ctx context.Context, path string, driveID driveid.ID) error
	UpsertActionableFailures(ctx context.Context, failures []syncstore.ActionableFailure) error
	ClearResolvedActionableFailures(ctx context.Context, issueType string, currentPaths []string) error
	ResetRetryTimesForScope(ctx context.Context, scopeKey synctypes.ScopeKey, now time.Time) error
}

type crashRecoveryStore interface {
	ResetDownloadingStates(ctx context.Context, delayFn func(int) time.Duration) error
	ListDeletingCandidates(ctx context.Context) ([]syncstore.RecoveryCandidate, error)
	FinalizeDeletingStates(
		ctx context.Context,
		deleted []syncstore.RecoveryCandidate,
		pending []syncstore.RecoveryCandidate,
		delayFn func(int) time.Duration,
	) error
}

type executionResultWriter interface {
	Load(ctx context.Context) (*syncstore.Baseline, error)
	CommitMutation(ctx context.Context, mutation *syncstore.BaselineMutation) error
}
