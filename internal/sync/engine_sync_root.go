package sync

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

func resolveExpectedSyncRoot(
	ctx context.Context,
	store *syncstore.SyncStore,
	tree *synctree.Root,
	parentIssued *synctree.FileIdentity,
) (syncdomain.ResolvedSyncRoot, error) {
	if parentIssued != nil {
		expected := synctree.RootIdentityFromFileIdentity(*parentIssued)
		if err := syncdomain.ValidateExpectedSyncRootIdentity(tree, &expected, ""); err != nil {
			return syncdomain.ResolvedSyncRoot{}, err
		}
		return syncdomain.ResolvedSyncRoot{Expected: &expected}, nil
	}

	state, err := store.ReadObservationState(ctx)
	if err != nil {
		return syncdomain.ResolvedSyncRoot{}, fmt.Errorf("sync: reading recorded sync folder identity: %w", err)
	}
	if state.SyncRootIdentity != nil {
		recorded := *state.SyncRootIdentity
		if mismatchErr := syncdomain.ValidateExpectedSyncRootIdentity(tree, &recorded, errSyncRootRebindHint); mismatchErr != nil {
			return syncdomain.ResolvedSyncRoot{}, mismatchErr
		}
		return syncdomain.ResolvedSyncRoot{Expected: &recorded}, nil
	}

	if !state.SyncRootRebind {
		synced, countErr := store.BaselineEntryCount(ctx)
		if countErr != nil {
			return syncdomain.ResolvedSyncRoot{}, fmt.Errorf("sync: counting synced items before binding the sync folder: %w", countErr)
		}
		if synced > 0 {
			// A state that already holds synced items but never recorded
			// its folder cannot prove the folder now at the sync path is
			// the one those items came from. Binding it silently would
			// accept exactly the unmounted or replaced folder this check
			// exists to refuse.
			return syncdomain.ResolvedSyncRoot{}, fmt.Errorf("%w: this sync state has no record of its sync folder; %s",
				syncdomain.ErrMountRootUnavailable, errSyncRootRebindHint)
		}
	}

	current, err := tree.RootIdentity()
	if err != nil {
		return syncdomain.ResolvedSyncRoot{}, fmt.Errorf("%w: %w", syncdomain.ErrMountRootUnavailable, err)
	}
	// A marker already on the folder belongs to a binding this state no
	// longer has (a reset-sync-state or rebind-root): the bind writes a new
	// one, so the stale token must not become the in-memory expectation.
	current.Token = ""

	return syncdomain.ResolvedSyncRoot{Expected: &current, BindPending: true}, nil
}

// errSyncRootRebindHint is appended to a primary-root mismatch so the user
// learns the one explicit way out. It is not offered for shortcut child roots,
// whose identity is owned by the parent engine.
const errSyncRootRebindHint = "if you moved or restored the sync folder on purpose, run " +
	"'onedrive-go drive rebind-root' to sync with the folder that is there now"
