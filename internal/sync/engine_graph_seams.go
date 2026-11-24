package sync

import (
	"context"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

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
