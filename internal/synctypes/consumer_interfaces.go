package synctypes

import (
	"context"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// ---------------------------------------------------------------------------
// Consumer-defined interfaces (satisfied by *graph.Client)
// ---------------------------------------------------------------------------

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
	ListChildren(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error)
	CreateFolder(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
	MoveItem(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error)
	DeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error
	PermanentDeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error
}

// DriveVerifier verifies that a configured drive ID is reachable and matches
// the remote API. Used at engine startup to detect stale config (B-074).
type DriveVerifier interface {
	Drive(ctx context.Context, driveID driveid.ID) (*graph.Drive, error)
}

// FolderDeltaFetcher provides folder-scoped delta enumeration for shortcut
// observation on personal drives (6.4b).
type FolderDeltaFetcher interface {
	DeltaFolderAll(ctx context.Context, driveID driveid.ID, folderID, token string) ([]graph.Item, string, error)
}

// RecursiveLister provides recursive children enumeration for shortcut
// observation on business/SharePoint drives where folder-scoped delta
// is not supported (6.4b).
type RecursiveLister interface {
	ListChildrenRecursive(ctx context.Context, driveID driveid.ID, folderID string) ([]graph.Item, error)
}

// PermissionChecker provides permission queries on drive items.
// Satisfied by *graph.Client.
type PermissionChecker interface {
	ListItemPermissions(ctx context.Context, driveID driveid.ID, itemID string) ([]graph.Permission, error)
}
