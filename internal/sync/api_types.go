package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	"github.com/tonimelisma/onedrive-go/internal/syncscope"
)

// LocalFilterConfig controls local-only observation exclusions. These filters
// affect what the scanner/watch pipeline turns into change events; they do not
// rewrite remote observation semantics.
type LocalFilterConfig struct {
	SkipDotfiles bool
	SkipSymlinks bool
	SkipDirs     []string
	SkipFiles    []string
}

// LocalObservationRules controls platform-derived local validation semantics.
// These are not user-configured exclusions; they encode rules that depend on
// the target drive type or sync surface.
type LocalObservationRules struct {
	RejectSharePointRootForms bool
}

type Mode = SyncMode

type (
	RunOptions struct {
		DryRun        bool
		FullReconcile bool // when true, runs a full delta enumeration + orphan detection
	}
	WatchOptions struct {
		PollInterval       time.Duration // remote delta polling interval (0 -> 5m)
		Debounce           time.Duration // buffer debounce window (0 -> 2s)
		SafetyScanInterval time.Duration // local safety scan interval (0 -> 5m) (B-099)
		ReconcileInterval  time.Duration // periodic full reconciliation (0 -> 24h, negative = disabled)
		MutationRequests   <-chan WatchMutationRequest
	}
	Report struct {
		Mode     Mode
		DryRun   bool
		Duration time.Duration

		FolderCreates  int
		Moves          int
		Downloads      int
		Uploads        int
		LocalDeletes   int
		RemoteDeletes  int
		Conflicts      int
		SyncedUpdates  int
		Cleanups       int
		DeferredByMode DeferredCounts

		Succeeded int
		Failed    int
		Errors    []error
	}
)

// DeltaFetcher fetches a page of delta changes from the Graph API.
type DeltaFetcher interface {
	Delta(ctx context.Context, driveID driveid.ID, token string) (*graph.DeltaPage, error)
}

// SocketIOEndpointFetcher fetches the outbound Socket.IO websocket endpoint
// used for near-real-time remote wakeups in watch mode.
type SocketIOEndpointFetcher interface {
	SocketIOEndpoint(ctx context.Context, driveID driveid.ID) (*graph.SocketIOEndpoint, error)
}

// ItemClient provides CRUD operations on drive items.
type ItemClient interface {
	GetItem(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error)
	GetItemByPath(ctx context.Context, driveID driveid.ID, remotePath string) (*graph.Item, error)
	ListChildren(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error)
	CreateFolder(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
	MoveItem(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error)
	DeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error
	PermanentDeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error
}

// DriveVerifier verifies that a configured drive ID is reachable and matches
// the remote API.
type DriveVerifier interface {
	Drive(ctx context.Context, driveID driveid.ID) (*graph.Drive, error)
}

// FolderDeltaFetcher provides folder-scoped delta enumeration for shortcut
// observation on personal drives.
type FolderDeltaFetcher interface {
	DeltaFolderAll(ctx context.Context, driveID driveid.ID, folderID, token string) ([]graph.Item, string, error)
}

// RecursiveLister provides recursive children enumeration for shortcut
// observation on business/SharePoint drives where folder-scoped delta is not supported.
type RecursiveLister interface {
	ListChildrenRecursive(ctx context.Context, driveID driveid.ID, folderID string) ([]graph.Item, error)
}

// PermissionChecker provides permission queries on drive items.
type PermissionChecker interface {
	ListItemPermissions(ctx context.Context, driveID driveid.ID, itemID string) ([]graph.Permission, error)
}

type engineInputs struct {
	DBPath                 string
	SyncRoot               string
	DataDir                string
	DriveID                driveid.ID
	DriveType              string
	AccountEmail           string
	RootItemID             string
	Fetcher                DeltaFetcher
	SocketIOFetcher        SocketIOEndpointFetcher
	Items                  ItemClient
	Downloads              driveops.Downloader
	Uploads                driveops.Uploader
	PathConvergenceFactory driveops.PathConvergenceFactory
	DriveVerifier          DriveVerifier
	FolderDelta            FolderDeltaFetcher
	RecursiveLister        RecursiveLister
	PermChecker            PermissionChecker
	Logger                 *slog.Logger
	LocalFilter            LocalFilterConfig
	LocalRules             LocalObservationRules
	SyncScope              syncscope.Config
	EnableWebsocket        bool
	UseLocalTrash          bool
	TransferWorkers        int
	CheckWorkers           int
	DeleteSafetyThreshold  int
	MinFreeSpace           int64
	PerfCollector          *perf.Collector
}
