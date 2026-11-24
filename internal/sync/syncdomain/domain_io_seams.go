package syncdomain

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
	// GetChildByName and ListChildrenPages look one name up in a folder: the
	// one read first, the listing a page at a time only when the read cannot
	// settle it, so a lookup does not cost every page of a large folder.
	GetChildByName(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
	ListChildrenPages(ctx context.Context, driveID driveid.ID, parentID string, visit graph.ChildrenPageFunc) error
	CreateFolder(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
	MoveItem(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string, keepModified time.Time) (*graph.Item, error)
	MoveItemIfMatch(
		ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string, opts graph.MoveOptions,
	) (*graph.Item, error)
	DeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error
	DeleteItemIfMatch(ctx context.Context, driveID driveid.ID, itemID, ifMatch string) error
	PermanentDeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error
}
