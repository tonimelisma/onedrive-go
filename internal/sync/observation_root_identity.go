package sync

import (
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// The local root identity check is an observation precondition: it decides
// whether a local scan may be trusted at all, before any snapshot is produced.

func validateExpectedSyncRootIdentity(root *synctree.Root, expected *synctree.FileIdentity) error {
	if root == nil || expected == nil {
		return nil
	}
	actual, err := root.IdentityNoFollow("")
	if err != nil {
		return fmt.Errorf("%w: %w", ErrMountRootUnavailable, err)
	}
	if !synctree.SameIdentity(actual, *expected) {
		// The folder at the sync path is not the one this state belongs to
		// (replaced, or a different disk mounted there): scanning it would
		// read as mass deletion.
		return fmt.Errorf("%w: it was replaced by a different folder", ErrMountRootUnavailable)
	}
	return nil
}
