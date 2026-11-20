package sync

import (
	"context"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// The local observer's test seams: what the engine's tests reach in an
// observer. A ...ForTest function is used only by tests and the sync
// test-helper packages.

// RunSafetyScanForTest runs one safety scan of tree (runSafetyScan).
func (o *LocalObserver) RunSafetyScanForTest(ctx context.Context, tree *synctree.Root) bool {
	return o.runSafetyScan(ctx, tree)
}

// MountBoundaryFlagsForTest is the set of folders flagged as crossed mount
// points; nil when none was ever set.
func (o *LocalObserver) MountBoundaryFlagsForTest() map[string]struct{} {
	flags := o.mountBoundaryFlags.Load()
	if flags == nil {
		return nil
	}

	return *flags
}
