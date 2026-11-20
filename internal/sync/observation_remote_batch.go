package sync

import "time"

// remoteObservationBatch is the value remote observation hands the watch loop
// to apply. It lives with observation rather than the engine because
// observation produces it; the loop is only its consumer.
type remoteObservationBatchSource string

const (
	remoteObservationBatchPrimaryWatch remoteObservationBatchSource = "primary_watch"
	remoteObservationBatchMountRoot    remoteObservationBatchSource = "mount_root_watch"
	remoteObservationBatchFullRefresh  remoteObservationBatchSource = "full_refresh"
)

type remoteObservationBatch struct {
	source                remoteObservationBatchSource
	observationMode       remoteObservationMode
	observed              []observedItem
	changes               []remoteChange
	cursorToken           string
	markFullRemoteRefresh bool
	// fullRefreshDueSoon pulls the full remote refresh forward to
	// fullRefreshAfterFolderDeleteDelay from now: the window holds a fact
	// only a full enumeration can complete (deltaWindowDeletesSyncedFolder).
	fullRefreshDueSoon bool
	// orphansUndecided marks a full refresh that kept an orphan it could not
	// confirm: the next one is due on the undecided-refresh backoff
	// (SyncStore.RecordFullRefreshOrphans).
	orphansUndecided      bool
	findings              ObservationFindingsBatch
	shortcutTopology      shortcutTopologyBatch
	armFullRefreshTimer   bool
	markFullRefreshIfIdle bool
	applyAck              chan error
	// fatalErr carries a panic recovered in the full-refresh goroutine. The
	// loop returns it instead of applying the batch, so the mount fails the
	// way an observer panic fails it (recoverObserverPanic).
	fatalErr error
	// pollStartedAt is when the watch poll that produced the batch began,
	// before its first page was asked for: the primary watch's on the
	// observer's clock, a mount-root engine's on the run's; zero for a batch
	// no such poll produced (a full refresh). Once the batch is applied, the
	// watch loop drops the fresh create intent of a file that is gone when
	// the last create attempt at its path ended before this time
	// (watchCreateAttempts).
	pollStartedAt time.Time
	// pollOnly marks the batch of a primary watch poll that returned no items:
	// it carries pollStartedAt and nothing to apply, neither items nor a
	// token, so it commits nothing and replans nothing.
	pollOnly bool
}
