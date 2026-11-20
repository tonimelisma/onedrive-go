package sync

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	stdsync "sync"
	"sync/atomic"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// ErrSyncRootRebindHint is appended to a primary-root mismatch so the user
// learns the one explicit way out. It is not offered for shortcut child roots,
// whose identity is owned by the parent engine.
const ErrSyncRootRebindHint = "if you moved or restored the sync folder on purpose, run " +
	"'onedrive-go drive rebind-root' to sync with the folder that is there now"

// ValidateExpectedSyncRootIdentity refuses a sync folder that is not the one
// expected. hint, when not empty, is appended to the refusal (the rebind hint
// for primary roots).
func ValidateExpectedSyncRootIdentity(root *synctree.Root, expected *synctree.RootIdentity, hint string) error {
	if root == nil || expected == nil {
		return nil
	}
	actual, err := root.RootIdentity()
	if err != nil {
		return withRefusalHint(fmt.Errorf("%w: %w", ErrMountRootUnavailable, err), hint)
	}
	if synctree.SameRoot(*expected, actual) {
		return nil
	}

	// The folder at the sync path is not the one this state belongs to:
	// scanning it would read as mass deletion. Say which evidence decided.
	var reason string
	switch {
	case expected.Token != "" && actual.Token == "":
		reason = "the folder has no sync folder marker (the extended attribute " + synctree.RootMarkAttribute +
			"); it was replaced, or copied or restored without extended attributes, or the disk it lives on is not mounted"
	case expected.Token != "":
		reason = "the folder carries a different sync folder marker (" + synctree.RootMarkAttribute +
			"); it is another sync folder, not the one this state was built from"
	default:
		reason = "it was replaced by a different folder, or the disk it lives on is not mounted"
	}
	return withRefusalHint(fmt.Errorf("%w: %s", ErrMountRootUnavailable, reason), hint)
}

func withRefusalHint(err error, hint string) error {
	if hint == "" {
		return err
	}
	return fmt.Errorf("%w; %s", err, hint)
}

// SyncRootIdentity is the one owner of the identity an engine's sync folder
// must match. The executor, every local observer and the binding read it
// through Expected, so the token a bind adds is seen everywhere at once
// rather than by whoever copied the identity after it (a stale copy refused
// every remote delete after a reset-sync-state rebind).
type SyncRootIdentity struct {
	current atomic.Pointer[synctree.RootIdentity]
	// refusalHint is appended to every refusal: the rebind hint for primary
	// roots, nothing for parent-owned shortcut child roots.
	refusalHint string
	// bindPending is set while a primary engine still has to record the
	// identity in its store.
	bindPending atomic.Bool
	// bindMu serializes mark-and-bind so two commits cannot write two
	// different tokens.
	bindMu stdsync.Mutex
}

func NewSyncRootIdentity(binding ResolvedSyncRoot, refusalHint string) *SyncRootIdentity {
	identity := &SyncRootIdentity{refusalHint: refusalHint}
	if binding.Expected != nil {
		expected := *binding.Expected
		identity.current.Store(&expected)
	}
	identity.bindPending.Store(binding.BindPending)
	return identity
}

// FixedSyncRootIdentity wraps a single known identity (tests, and callers
// that validate once).
func FixedSyncRootIdentity(expected *synctree.RootIdentity) *SyncRootIdentity {
	return NewSyncRootIdentity(ResolvedSyncRoot{Expected: expected}, "")
}

// Expected returns a copy of the identity to validate against, or nil.
func (s *SyncRootIdentity) Expected() *synctree.RootIdentity {
	if s == nil {
		return nil
	}
	expected := s.current.Load()
	if expected == nil {
		return nil
	}
	copied := *expected
	return &copied
}

// Validate refuses tree when it is not the expected sync folder.
func (s *SyncRootIdentity) Validate(tree *synctree.Root) error {
	if s == nil {
		return nil
	}
	return ValidateExpectedSyncRootIdentity(tree, s.Expected(), s.refusalHint)
}

// ResolvedSyncRoot is the expected identity an engine validates every scan
// against, plus whether the store still has to record it.
type ResolvedSyncRoot struct {
	Expected *synctree.RootIdentity
	// bindPending is true for a primary engine whose store has no recorded
	// fingerprint yet. The fingerprint taken at startup is enforced in memory
	// immediately, and persisted with the first committed local snapshot so a
	// dry run never writes it.
	BindPending bool
}

// CountSyncFolderFiles counts the regular files under syncDir without
// following links, so `drive rebind-root` can show the user what they are
// about to bind against what the state remembers. A missing folder holds zero
// files.
func CountSyncFolderFiles(syncDir string) (int, error) {
	tree, err := synctree.Open(syncDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("opening sync folder: %w", err)
	}
	files := 0
	walkErr := tree.WalkDir(func(_ string, d fs.DirEntry, entryErr error) error {
		if entryErr != nil {
			return nil //nolint:nilerr // an unreadable entry is simply not counted
		}
		if d.Type().IsRegular() {
			files++
		}
		return nil
	})
	if walkErr != nil {
		if errors.Is(walkErr, os.ErrNotExist) {
			return 0, nil
		}
		return files, fmt.Errorf("counting files in sync folder: %w", walkErr)
	}
	return files, nil
}
