package sync

import (
	"context"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// The I/O seams the sync families depend on. They are declared here rather than
// beside the engine because observation, execution, and the engine all consume
// them: filing them with the engine made the two lower families appear to
// depend on runtime orchestration when they depend only on the shape of the
// calls they make.

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
