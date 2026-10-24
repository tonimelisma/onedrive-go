package sync

import (
	"log/slog"
	"sync/atomic"
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

	// rootInaccessibleReported is set once an inaccessible sync folder has
	// been warned about, and cleared when it is accessible again, so a
	// queue of failing actions warns once rather than once per action.
	rootInaccessibleReported atomic.Bool
}
