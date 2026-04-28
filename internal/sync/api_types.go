package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// LocalFilterConfig controls local-only observation exclusions. These filters
// affect what the scanner/watch pipeline turns into change events; they do not
// rewrite remote observation semantics. SkipDirs entries are rooted relative
// slash paths beneath the mount root, not basename-only directory names.
type LocalFilterConfig struct {
	SkipDotfiles bool
	SkipSymlinks bool
	SkipDirs     []string
	SkipFiles    []string
}

// ProtectedRoot marks a parent-engine protected subtree inside this
// mount. The sync engine suppresses normal local content events for these roots
// and, when it can identify the same directory at a sibling path, wakes the
// parent engine shortcut-root lifecycle instead of uploading a duplicate folder.
type ProtectedRoot struct {
	Path           string
	MountID        string
	BindingID      string
	RemoteDriveID  driveid.ID
	RemoteItemID   string
	RemoteIsFolder bool
	Device         uint64
	Inode          uint64
	HasIdentity    bool
}

// ProtectedRootEventType identifies the lifecycle fact the local observer found.
type ProtectedRootEventType string

const (
	ProtectedRootEventPathReserved  ProtectedRootEventType = "path_reserved"
	ProtectedRootEventIdentityMatch ProtectedRootEventType = "identity_match"
)

// ProtectedRootEvent is a narrow parent-engine-internal notification from the
// local observer to the watch runtime. It never plans child content work; it
// only wakes the parent shortcut-root lifecycle so parent-owned state can
// publish updated child work commands promptly.
type ProtectedRootEvent struct {
	Type         ProtectedRootEventType
	Path         string
	ReservedPath string
	MountID      string
	BindingID    string
}

type ProtectedRootEventSink func(ProtectedRootEvent)

// LocalObservationRules controls platform-derived local validation semantics.
// These are not user-configured exclusions; they encode rules that depend on
// the target drive type or sync surface.
type LocalObservationRules struct {
	RejectSharePointRootForms bool
}

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

// DriveVerifier verifies that a content drive ID is reachable and matches
// the remote API.
type DriveVerifier interface {
	Drive(ctx context.Context, driveID driveid.ID) (*graph.Drive, error)
}

// FolderDeltaFetcher provides mount-root delta enumeration for engines
// rooted below the remote drive root.
type FolderDeltaFetcher interface {
	DeltaFolderAll(ctx context.Context, driveID driveid.ID, folderID, token string) ([]graph.Item, string, error)
}

// RecursiveLister provides recursive children enumeration for mount-root
// observation when mount-root delta is not supported.
type RecursiveLister interface {
	ListChildrenRecursive(ctx context.Context, driveID driveid.ID, folderID string) ([]graph.Item, error)
}

// PermissionChecker provides permission queries on drive items.
type PermissionChecker interface {
	ListItemPermissions(ctx context.Context, driveID driveid.ID, itemID string) ([]graph.Permission, error)
}

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
	Fetcher                  DeltaFetcher
	SocketIOFetcher          SocketIOEndpointFetcher
	Items                    ItemClient
	Downloads                driveops.Downloader
	Uploads                  driveops.Uploader
	PathConvergence          driveops.PathConvergence
	DriveVerifier            DriveVerifier
	FolderDelta              FolderDeltaFetcher
	RecursiveLister          RecursiveLister
	PermChecker              PermissionChecker
	Logger                   *slog.Logger
	LocalFilter              LocalFilterConfig
	LocalRules               LocalObservationRules
	ShortcutNamespaceID      string
	ShortcutChildWorkSink    ShortcutChildWorkSink
	EnableWebsocket          bool
	TransferWorkers          int
	CheckWorkers             int
	MinFreeSpace             int64
	PerfCollector            *perf.Collector
}
