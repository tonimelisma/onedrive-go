package sync

import (
	"context"
	stdsync "sync"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// protectedRootSet owns the engine's protected roots: the sync-folder paths
// of the shortcut child mounts below this one, which observation and
// housekeeping must not treat as this mount's content (R-2.4.10).
//
// It is refreshed on the loop goroutine before every observation and before
// housekeeping, and off it from the full-refresh goroutine, the remote
// observer's expired-delta recovery and the multisync orchestrator's
// goroutine, whose final-drain acknowledgement through the ack handle
// reconciles the shortcut roots (reconcileShortcutRootLocalState), so its
// roots are read and written under its lock (R-6.3.4).
type protectedRootSet struct {
	store       *syncstore.SyncStore
	namespaceID string

	mu    stdsync.Mutex // guards roots
	roots []syncshortcut.ProtectedRoot
}

// newProtectedRootSet is the protected roots of the shortcut roots store
// holds under namespaceID; empty until the first refresh.
func newProtectedRootSet(store *syncstore.SyncStore, namespaceID string) *protectedRootSet {
	return &protectedRootSet{store: store, namespaceID: namespaceID}
}

// refresh rederives the protected roots from the persisted shortcut-root
// state. A mount without a shortcut namespace protects nothing.
func (s *protectedRootSet) refresh(ctx context.Context) error {
	if s.namespaceID == "" {
		s.set(nil)
		return nil
	}
	records, err := s.store.ListShortcutRoots(ctx)
	if err != nil {
		return err
	}
	s.set(syncshortcut.ProtectedRootsForShortcutRoots(records, s.namespaceID))

	return nil
}

// set replaces the protected roots.
func (s *protectedRootSet) set(roots []syncshortcut.ProtectedRoot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roots = roots
}

// current returns a copy of the protected roots, so a reader keeps a stable
// list while a concurrent refresh replaces them.
func (s *protectedRootSet) current() []syncshortcut.ProtectedRoot {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.roots == nil {
		return nil
	}

	return append([]syncshortcut.ProtectedRoot(nil), s.roots...)
}
