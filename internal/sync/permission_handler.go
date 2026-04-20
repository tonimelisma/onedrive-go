package sync

import (
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// PermissionHandler owns permission probing and revalidation. It does not
// persist durable state itself; the engine applies the returned decisions.
type PermissionHandler struct {
	store        *SyncStore
	permChecker  PermissionChecker
	syncTree     *synctree.Root
	driveID      driveid.ID
	accountEmail string
	rootItemID   string
	logger       *slog.Logger
	nowFn        func() time.Time
}

type remoteBoundaryRoot struct {
	remoteDrive string
	remoteItem  string
	localPath   string
}

// HasPermChecker reports whether a remote permission checker is configured.
func (ph *PermissionHandler) HasPermChecker() bool {
	return ph.permChecker != nil
}
