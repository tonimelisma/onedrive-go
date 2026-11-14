package sync

import (
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

type (
	RunOptions struct {
		DryRun        bool
		FullReconcile bool // when true, runs a full delta enumeration + orphan detection
		// ConfirmDeletes releases the deletes the delete-safety hold
		// recorded before this pass (`sync --confirm-deletes`); deletes that
		// appeared since are judged by the hold again.
		ConfirmDeletes bool
	}
	WatchOptions struct {
		PollInterval time.Duration // remote delta polling interval (0 -> 5m)
		Debounce     time.Duration // local/remote observation debounce window before replanning (0 -> 5s)
	}
	// Report is what one RunOnce did, over all its passes (withPass). The
	// counts of work done add up over the passes: the action counts by type,
	// Succeeded, Failed, Superseded and Errors. The rest is the state the
	// last pass ended in: DeferredByMode, the held deletes, LeftForNextSync,
	// LastPassFailed, LastPassKeptForRetry, and the counts read from the store
	// after it (UnsyncedIssues, InterruptedRemovals). What the run left
	// (LeftWork) and whether its local tree may be removed (LocalTreeHolds)
	// are read from that state alone.
	Report struct {
		Mode     SyncMode
		DryRun   bool
		Duration time.Duration

		FolderCreates   int
		Moves           int
		Downloads       int
		Uploads         int
		LocalDeletes    int
		RemoteDeletes   int
		ConflictCopies  int
		BaselineUpdates int
		Cleanups        int
		DeferredByMode  DeferredCounts
		// HeldLocalDeletes and HeldRemoteDeletes count the deletes the
		// delete-safety hold kept for confirmation, by the side they would
		// remove content from.
		HeldLocalDeletes  int
		HeldRemoteDeletes int
		// LeftForNextSync is the planned work the last pass neither
		// completed nor failed (actions a throttle scope took back and holds
		// for a trial after the pass ended, or parked retry work), plus the
		// follow-up work of the plan the run ended on
		// (planFollowUp.leftWhenTheRunEnds). The summary counts it with the
		// retryable failures as "will be retried".
		LeftForNextSync int
		// Superseded counts actions the pass gave up on because the truth
		// they were planned against changed under them (an ETag refused by
		// a 412, an item gone from where the plan put it); each is replanned
		// by the next pass, which a one-shot run makes itself (R-2.1.7).
		Superseded int
		// UnsyncedIssues is how many observation issues of a type that leaves
		// a file unsynced the store holds after a live run, read from the
		// snapshot `status` reads (R-6.6.28). They are the work left that no
		// action counts: every retry row a live pass leaves belongs to an
		// action of that pass's plan, which the pass counts as failed or left.
		// A dry run leaves it zero: it attempted nothing.
		UnsyncedIssues int
		// InterruptedRemovals is how many files the store reports sync set
		// aside while removing them and never moved out
		// (local_interrupted_removal): the user's content under an internal
		// name, which sync neither syncs nor deletes. It keeps no file
		// unsynced, so it is not work left, but it keeps the local tree from
		// being removed (LocalTreeHolds). Read with UnsyncedIssues.
		InterruptedRemovals int

		Succeeded int
		// Failed is every failed action of every pass. What the run left is
		// counted from the last pass instead (LastPassFailed).
		Failed int
		// LastPassFailed is the failed actions of the run's last pass, and
		// LastPassKeptForRetry the part of them the engine keeps as retry work
		// it runs again by itself (a transient failure, or one a scope holds
		// until its trial).
		LastPassFailed       int
		LastPassKeptForRetry int
		Errors               []error
	}
)

// engineInputs is everything newEngine needs to build one mount's engine.
type engineInputs struct {
	MountID                  string
	DBPath                   string
	SyncRoot                 string
	DataDir                  string
	DriveID                  driveid.ID
	DriveType                string
	AccountEmail             string
	RemoteRootItemID         string
	RemoteRootDeltaCapable   bool
	ExpectedSyncRootIdentity *synctree.FileIdentity
	Fetcher                  deltaFetcher
	SocketIOFetcher          socketIOEndpointFetcher
	Items                    itemClient
	Downloads                driveops.Downloader
	Uploads                  driveops.Uploader
	PathConvergence          driveops.PathConvergence
	DriveVerifier            driveVerifier
	FolderDelta              folderDeltaFetcher
	RecursiveLister          recursiveLister
	PermChecker              permissionChecker
	Logger                   *slog.Logger
	ContentFilter            ContentFilterConfig
	LocalRules               LocalObservationRules
	ShortcutNamespaceID      string
	ShortcutChildWorkSink    ShortcutChildWorkSink
	EnableWebsocket          bool
	TransferWorkers          int
	CheckWorkers             int
	MinFreeSpace             int64
	PerfCollector            *perf.Collector

	// lockFlock replaces syscall.Flock for the engine's two locks, on its sync
	// folder and on its state store; nil in production. Tests inject
	// unsupported-filesystem errnos through it and watch the unlocks.
	lockFlock func(fd int, how int) error
	// interposeSyncTree wraps the sync tree's filesystem handle; nil in
	// production. Tests put an emulated filesystem under the engine through
	// it: a case-insensitive volume, injected errno (D-094).
	interposeSyncTree func(synctree.Handle) synctree.Handle
}
