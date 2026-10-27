package sync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	stdsync "sync"
	"sync/atomic"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// The local root identity check is an observation precondition: it decides
// whether a local scan may be trusted at all, before any snapshot is produced.
//
// Every engine has an expected identity. A shortcut child engine gets it from
// its parent, which owns the alias folder. A primary engine gets it from its
// own store: the fingerprint recorded with the first committed local
// snapshot. Without that, an unmounted disk, a wiped tmpfs, or a symlinked
// root re-pointed at an empty folder scans successfully with zero entries,
// and every synced file is planned as a remote delete (D-001).

// errSyncRootRebindHint is appended to a primary-root mismatch so the user
// learns the one explicit way out. It is not offered for shortcut child roots,
// whose identity is owned by the parent engine.
const errSyncRootRebindHint = "if you moved or restored the sync folder on purpose, run " +
	"'onedrive-go drive rebind-root' to sync with the folder that is there now"

// validateExpectedSyncRootIdentity refuses a sync folder that is not the one
// expected. hint, when not empty, is appended to the refusal (the rebind hint
// for primary roots).
func validateExpectedSyncRootIdentity(root *synctree.Root, expected *synctree.RootIdentity, hint string) error {
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

// syncRootIdentity is the one owner of the identity an engine's sync folder
// must match. The executor, every local observer and the binding read it
// through Expected, so the token a bind adds is seen everywhere at once
// rather than by whoever copied the identity after it (a stale copy refused
// every remote delete after a reset-sync-state rebind).
type syncRootIdentity struct {
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

func newSyncRootIdentity(binding resolvedSyncRoot, refusalHint string) *syncRootIdentity {
	identity := &syncRootIdentity{refusalHint: refusalHint}
	if binding.expected != nil {
		expected := *binding.expected
		identity.current.Store(&expected)
	}
	identity.bindPending.Store(binding.bindPending)
	return identity
}

// fixedSyncRootIdentity wraps a single known identity (tests, and callers
// that validate once).
func fixedSyncRootIdentity(expected *synctree.RootIdentity) *syncRootIdentity {
	return newSyncRootIdentity(resolvedSyncRoot{expected: expected}, "")
}

// Expected returns a copy of the identity to validate against, or nil.
func (s *syncRootIdentity) Expected() *synctree.RootIdentity {
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
func (s *syncRootIdentity) Validate(tree *synctree.Root) error {
	if s == nil {
		return nil
	}
	return validateExpectedSyncRootIdentity(tree, s.Expected(), s.refusalHint)
}

// resolvedSyncRoot is the expected identity an engine validates every scan
// against, plus whether the store still has to record it.
type resolvedSyncRoot struct {
	expected *synctree.RootIdentity
	// bindPending is true for a primary engine whose store has no recorded
	// fingerprint yet. The fingerprint taken at startup is enforced in memory
	// immediately, and persisted with the first committed local snapshot so a
	// dry run never writes it.
	bindPending bool
}

func resolveExpectedSyncRoot(
	ctx context.Context,
	store *SyncStore,
	tree *synctree.Root,
	parentIssued *synctree.FileIdentity,
) (resolvedSyncRoot, error) {
	if parentIssued != nil {
		expected := synctree.RootIdentityFromFileIdentity(*parentIssued)
		if err := validateExpectedSyncRootIdentity(tree, &expected, ""); err != nil {
			return resolvedSyncRoot{}, err
		}
		return resolvedSyncRoot{expected: &expected}, nil
	}

	state, err := store.ReadObservationState(ctx)
	if err != nil {
		return resolvedSyncRoot{}, fmt.Errorf("sync: reading recorded sync folder identity: %w", err)
	}
	if state.SyncRootIdentity != nil {
		recorded := *state.SyncRootIdentity
		if mismatchErr := validateExpectedSyncRootIdentity(tree, &recorded, errSyncRootRebindHint); mismatchErr != nil {
			return resolvedSyncRoot{}, mismatchErr
		}
		return resolvedSyncRoot{expected: &recorded}, nil
	}

	if !state.SyncRootRebind {
		synced, countErr := store.baselineEntryCount(ctx)
		if countErr != nil {
			return resolvedSyncRoot{}, fmt.Errorf("sync: counting synced items before binding the sync folder: %w", countErr)
		}
		if synced > 0 {
			// A state that already holds synced items but never recorded
			// its folder cannot prove the folder now at the sync path is
			// the one those items came from. Binding it silently would
			// accept exactly the unmounted or replaced folder this check
			// exists to refuse.
			return resolvedSyncRoot{}, fmt.Errorf("%w: this sync state has no record of its sync folder; %s",
				ErrMountRootUnavailable, errSyncRootRebindHint)
		}
	}

	current, err := tree.RootIdentity()
	if err != nil {
		return resolvedSyncRoot{}, fmt.Errorf("%w: %w", ErrMountRootUnavailable, err)
	}
	// A marker already on the folder belongs to a binding this state no
	// longer has (a reset-sync-state or rebind-root): the bind writes a new
	// one, so the stale token must not become the in-memory expectation.
	current.Token = ""

	return resolvedSyncRoot{expected: &current, bindPending: true}, nil
}

// resolveSyncRootPath returns the folder the sync tree is opened on. When the
// configured sync folder is itself a symbolic link, the tree is opened on its
// target, resolved once at startup: re-pointing the link while the engine runs
// must not silently move every later operation to a different folder. Only the
// final component is resolved, so platform aliases in ancestors (such as
// macOS's /var -> /private/var) keep the configured spelling.
func resolveSyncRootPath(syncRoot string) (string, error) {
	info, err := localpath.Lstat(syncRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return syncRoot, nil
		}
		return "", fmt.Errorf("checking sync folder %s: %w", syncRoot, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return syncRoot, nil
	}

	resolved, err := filepath.EvalSymlinks(syncRoot)
	if err != nil {
		return "", fmt.Errorf("%w: resolving symbolic link %s: %w", ErrMountRootUnavailable, syncRoot, err)
	}

	return resolved, nil
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

// SyncedFileCount reports how many files the drive's sync state at dbPath
// records as synced.
func SyncedFileCount(ctx context.Context, dbPath string, logger *slog.Logger) (int, error) {
	if _, statErr := localpath.Stat(dbPath); errors.Is(statErr, os.ErrNotExist) {
		return 0, nil
	}
	snapshot, err := ReadDriveStatusSnapshot(ctx, dbPath, logger)
	if err != nil {
		return 0, err
	}
	return snapshot.BaselineFileCount, nil
}
