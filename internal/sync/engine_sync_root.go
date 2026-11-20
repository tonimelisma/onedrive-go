package sync

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

func resolveExpectedSyncRoot(
	ctx context.Context,
	store *SyncStore,
	tree *synctree.Root,
	parentIssued *synctree.FileIdentity,
) (ResolvedSyncRoot, error) {
	if parentIssued != nil {
		expected := synctree.RootIdentityFromFileIdentity(*parentIssued)
		if err := ValidateExpectedSyncRootIdentity(tree, &expected, ""); err != nil {
			return ResolvedSyncRoot{}, err
		}
		return ResolvedSyncRoot{Expected: &expected}, nil
	}

	state, err := store.ReadObservationState(ctx)
	if err != nil {
		return ResolvedSyncRoot{}, fmt.Errorf("sync: reading recorded sync folder identity: %w", err)
	}
	if state.SyncRootIdentity != nil {
		recorded := *state.SyncRootIdentity
		if mismatchErr := ValidateExpectedSyncRootIdentity(tree, &recorded, ErrSyncRootRebindHint); mismatchErr != nil {
			return ResolvedSyncRoot{}, mismatchErr
		}
		return ResolvedSyncRoot{Expected: &recorded}, nil
	}

	if !state.SyncRootRebind {
		synced, countErr := store.BaselineEntryCount(ctx)
		if countErr != nil {
			return ResolvedSyncRoot{}, fmt.Errorf("sync: counting synced items before binding the sync folder: %w", countErr)
		}
		if synced > 0 {
			// A state that already holds synced items but never recorded
			// its folder cannot prove the folder now at the sync path is
			// the one those items came from. Binding it silently would
			// accept exactly the unmounted or replaced folder this check
			// exists to refuse.
			return ResolvedSyncRoot{}, fmt.Errorf("%w: this sync state has no record of its sync folder; %s",
				ErrMountRootUnavailable, ErrSyncRootRebindHint)
		}
	}

	current, err := tree.RootIdentity()
	if err != nil {
		return ResolvedSyncRoot{}, fmt.Errorf("%w: %w", ErrMountRootUnavailable, err)
	}
	// A marker already on the folder belongs to a binding this state no
	// longer has (a reset-sync-state or rebind-root): the bind writes a new
	// one, so the stale token must not become the in-memory expectation.
	current.Token = ""

	return ResolvedSyncRoot{Expected: &current, BindPending: true}, nil
}
