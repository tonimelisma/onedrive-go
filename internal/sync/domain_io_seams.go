package sync

import (
	"context"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// The I/O seams the sync families depend on. They are declared here rather than
// beside the engine because observation, execution, and the engine all consume
// them: filing them with the engine made the two lower families appear to
// depend on runtime orchestration when they depend only on the shape of the
// calls they make.

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
	MoveItem(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string, keepModified time.Time) (*graph.Item, error)
	MoveItemIfMatch(
		ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string, opts graph.MoveOptions,
	) (*graph.Item, error)
	DeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error
	DeleteItemIfMatch(ctx context.Context, driveID driveid.ID, itemID, ifMatch string) error
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
