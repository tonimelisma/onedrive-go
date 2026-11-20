package sync

import (
	"context"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
)

// refreshProtectedRootsFromStore rederives the protected roots from the
// persisted shortcut-root state. It runs on the loop goroutine before every
// observation and before housekeeping, and off it from the full-refresh
// goroutine, the remote observer's expired-delta recovery and the multisync
// orchestrator's goroutine, whose final-drain acknowledgement through the ack
// handle reconciles the shortcut roots (reconcileShortcutRootLocalState), so
// the write goes through the lock.
func (e *Engine) refreshProtectedRootsFromStore(ctx context.Context) error {
	if e == nil || e.baseline == nil {
		return nil
	}
	if e.shortcutNamespaceID == "" {
		e.setProtectedRoots(nil)
		return nil
	}
	records, err := e.baseline.ListShortcutRoots(ctx)
	if err != nil {
		return err
	}
	e.setProtectedRoots(syncshortcut.ProtectedRootsForShortcutRoots(records, e.shortcutNamespaceID))
	return nil
}

// setProtectedRoots replaces the protected roots under protectedRootsMu.
func (e *Engine) setProtectedRoots(roots []syncshortcut.ProtectedRoot) {
	e.protectedRootsMu.Lock()
	defer e.protectedRootsMu.Unlock()
	e.protectedRoots = roots
}

// currentProtectedRoots returns a copy of the protected roots, so a reader
// keeps a stable list while a concurrent refresh replaces the engine's.
func (e *Engine) currentProtectedRoots() []syncshortcut.ProtectedRoot {
	e.protectedRootsMu.Lock()
	defer e.protectedRootsMu.Unlock()
	if e.protectedRoots == nil {
		return nil
	}

	return append([]syncshortcut.ProtectedRoot(nil), e.protectedRoots...)
}
