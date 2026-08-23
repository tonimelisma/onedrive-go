package sync

import (
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// permissionHandler owns permission probing and revalidation. It gathers
// evidence only; the engine-owned runtime handlers turn that evidence into
// durable retry/scope mutations.
type permissionHandler struct {
	store            *SyncStore
	permChecker      permissionChecker
	syncTree         *synctree.Root
	driveID          driveid.ID
	accountEmail     string
	remoteRootItemID string
	logger           *slog.Logger
	nowFn            func() time.Time
}
