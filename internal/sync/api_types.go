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

// protectedRootEventType identifies the lifecycle fact the local observer found.
type protectedRootEventType string

const (
	protectedRootEventPathReserved  protectedRootEventType = "path_reserved"
	protectedRootEventIdentityMatch protectedRootEventType = "identity_match"
)

// protectedRootEvent is a narrow parent-engine-internal notification from the
// local observer to the watch runtime. It never plans child content work; it
// only wakes the parent shortcut-root lifecycle so parent-owned state can
// publish updated child work commands promptly.
type protectedRootEvent struct {
	Type         protectedRootEventType
	Path         string
	ReservedPath string
	MountID      string
	BindingID    string
}

type protectedRootEventSink func(protectedRootEvent)

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
type deltaFetcher interface {
	Delta(ctx context.Context, driveID driveid.ID, token string) (*graph.DeltaPage, error)
}

// socketIOEndpointFetcher fetches the outbound Socket.IO websocket endpoint
// used for near-real-time remote wakeups in watch mode.
type socketIOEndpointFetcher interface {
	SocketIOEndpoint(ctx context.Context, driveID driveid.ID) (*graph.SocketIOEndpoint, error)
}

// itemClient provides CRUD operations on drive items.
type itemClient interface {
	GetItem(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error)
	GetItemByPath(ctx context.Context, driveID driveid.ID, remotePath string) (*graph.Item, error)
	ListChildren(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error)
	CreateFolder(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
	MoveItem(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error)
	MoveItemIfMatch(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName, ifMatch string) (*graph.Item, error)
	DeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error
	DeleteItemIfMatch(ctx context.Context, driveID driveid.ID, itemID, ifMatch string) error
	PermanentDeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error
}

// driveVerifier verifies that a content drive ID is reachable and matches
// the remote API.
type driveVerifier interface {
	Drive(ctx context.Context, driveID driveid.ID) (*graph.Drive, error)
}

// folderDeltaFetcher provides mount-root delta enumeration for engines
// rooted below the remote drive root.
type folderDeltaFetcher interface {
	DeltaFolderAll(ctx context.Context, driveID driveid.ID, folderID, token string) ([]graph.Item, string, error)
}

// recursiveLister provides recursive children enumeration for mount-root
// observation when mount-root delta is not supported.
type recursiveLister interface {
	ListChildrenRecursive(ctx context.Context, driveID driveid.ID, folderID string) ([]graph.Item, error)
}

// permissionChecker provides permission queries on drive items.
type permissionChecker interface {
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
