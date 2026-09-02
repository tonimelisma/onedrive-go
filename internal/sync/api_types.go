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
	}
	WatchOptions struct {
		PollInterval time.Duration // remote delta polling interval (0 -> 5m)
		Debounce     time.Duration // local/remote observation debounce window before replanning (0 -> 5s)
	}
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

		Succeeded int
		Failed    int
		Errors    []error
	}
)

// deltaFetcher fetches a page of delta changes from the Graph API.
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
}
