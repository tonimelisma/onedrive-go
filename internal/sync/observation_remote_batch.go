package sync

import (
	"context"
	"fmt"
	"time"
)

// RemoteObservationBatch is the value remote observation hands the watch loop
// to apply. It lives with observation rather than the engine because
// observation produces it; the loop is only its consumer.
type RemoteObservationBatchSource string

const (
	RemoteObservationBatchPrimaryWatch RemoteObservationBatchSource = "primary_watch"
	RemoteObservationBatchMountRoot    RemoteObservationBatchSource = "mount_root_watch"
	RemoteObservationBatchFullRefresh  RemoteObservationBatchSource = "full_refresh"
)

type RemoteObservationBatch struct {
	Source                RemoteObservationBatchSource
	ObservationMode       RemoteObservationMode
	Observed              []ObservedItem
	Changes               []RemoteChange
	CursorToken           string
	MarkFullRemoteRefresh bool
	// fullRefreshDueSoon pulls the full remote refresh forward to
	// FullRefreshAfterFolderDeleteDelay from now: the window holds a fact
	// only a full enumeration can complete (deltaWindowDeletesSyncedFolder).
	FullRefreshDueSoon bool
	// orphansUndecided marks a full refresh that kept an orphan it could not
	// confirm: the next one is due on the undecided-refresh backoff
	// (SyncStore.RecordFullRefreshOrphans).
	OrphansUndecided      bool
	Findings              ObservationFindingsBatch
	ShortcutTopology      ShortcutTopologyBatch
	ArmFullRefreshTimer   bool
	MarkFullRefreshIfIdle bool
	ApplyAck              chan error
	// fatalErr carries a panic recovered in the full-refresh goroutine. The
	// loop returns it instead of applying the batch, so the mount fails the
	// way an observer panic fails it (recoverObserverPanic).
	FatalErr error
	// pollStartedAt is when the watch poll that produced the batch began,
	// before its first page was asked for: the primary watch's on the
	// observer's clock, a mount-root engine's on the run's; zero for a batch
	// no such poll produced (a full refresh). Once the batch is applied, the
	// watch loop drops the fresh create intent of a file that is gone when
	// the last create attempt at its path ended before this time
	// (watchCreateAttempts).
	PollStartedAt time.Time
	// pollOnly marks the batch of a primary watch poll that returned no items:
	// it carries pollStartedAt and nothing to apply, neither items nor a
	// token, so it commits nothing and replans nothing.
	PollOnly bool
}

func (batch *RemoteObservationBatch) WaitApplied(ctx context.Context) error {
	if batch == nil || batch.ApplyAck == nil {
		return nil
	}

	select {
	case err := <-batch.ApplyAck:
		return err
	case <-ctx.Done():
		return fmt.Errorf("waiting for remote observation batch apply: %w", ctx.Err())
	}
}

func (batch *RemoteObservationBatch) FinishApplied(err error) {
	if batch == nil || batch.ApplyAck == nil {
		return
	}

	select {
	case batch.ApplyAck <- err:
	default:
	}
}

func (batch *RemoteObservationBatch) HasObservationFindings() bool {
	if batch == nil {
		return false
	}

	return len(batch.Findings.Issues) > 0 ||
		len(batch.Findings.ManagedIssueTypes) > 0
}

func (batch *RemoteObservationBatch) DeferredProgress() *RemoteObservationBatch {
	if batch == nil ||
		(batch.CursorToken == "" &&
			!batch.MarkFullRemoteRefresh &&
			!batch.FullRefreshDueSoon &&
			!batch.OrphansUndecided &&
			batch.ObservationMode != RemoteObservationModeEnumerate) {
		return nil
	}

	clone := *batch
	clone.Source = ""
	clone.Observed = nil
	clone.Changes = nil
	clone.Findings = ObservationFindingsBatch{}
	clone.ShortcutTopology = ShortcutTopologyBatch{}
	clone.ArmFullRefreshTimer = false
	clone.MarkFullRefreshIfIdle = false
	clone.ApplyAck = nil

	return &clone
}
