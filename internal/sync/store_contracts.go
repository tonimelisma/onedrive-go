package sync

import (
	"context"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

type observationWriter interface {
	CommitObservation(ctx context.Context, events []ObservedItem, newToken string, driveID driveid.ID) error
}

type failureRecorder interface {
	RecordFailure(ctx context.Context, p *SyncFailureParams, delayFn func(int) time.Duration) error
	ListActionableFailures(ctx context.Context) ([]SyncFailureRow, error)
	ListSyncFailuresByIssueType(ctx context.Context, issueType string) ([]SyncFailureRow, error)
	ListRemoteBlockedFailures(ctx context.Context) ([]SyncFailureRow, error)
	ClearSyncFailure(ctx context.Context, path string, driveID driveid.ID) error
	ClearActionableSyncFailures(ctx context.Context) error
	MarkSyncFailureActionable(ctx context.Context, path string, driveID driveid.ID) error
	UpsertActionableFailures(ctx context.Context, failures []ActionableFailure) error
	ClearResolvedActionableFailures(ctx context.Context, issueType string, currentPaths []string) error
	ResetRetryTimesForScope(ctx context.Context, scopeKey ScopeKey, now time.Time) error
}

type executionResultWriter interface {
	Load(ctx context.Context) (*Baseline, error)
	CommitMutation(ctx context.Context, mutation *BaselineMutation) error
}
