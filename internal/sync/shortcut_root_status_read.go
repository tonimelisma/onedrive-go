package sync

import (
	"context"
	"fmt"
	"log/slog"
)

// Reading parent shortcut-root status is shortcut-owned even though it goes
// through the store's read-only inspector: the view it returns and the
// lifecycle policy it renders belong to the shortcut family, and putting the
// entry point in store_inspect.go made the store depend on them.

// ReadShortcutRootStatusSnapshot opens a read-only inspector for parent-owned
// shortcut-root lifecycle state and closes it before returning.
func ReadShortcutRootStatusSnapshot(
	ctx context.Context,
	dbPath string,
	namespaceID string,
	parentSyncRoot string,
	logger *slog.Logger,
) ([]ShortcutRootStatusView, error) {
	return readWithInspector(dbPath, logger, func(inspector *storeInspector) ([]ShortcutRootStatusView, error) {
		return inspector.ReadShortcutRootStatusSnapshot(ctx, namespaceID, parentSyncRoot)
	})
}

// ReadShortcutRootStatusSnapshot reads parent-owned shortcut-root lifecycle
// state from an already-open sync store.
func (s *SyncStore) ReadShortcutRootStatusSnapshot(
	ctx context.Context,
	namespaceID string,
	parentSyncRoot string,
) ([]ShortcutRootStatusView, error) {
	if s == nil {
		return nil, fmt.Errorf("read shortcut root status snapshot: nil store")
	}

	inspector := &storeInspector{
		db:     s.db,
		logger: s.logger,
	}

	return inspector.ReadShortcutRootStatusSnapshot(ctx, namespaceID, parentSyncRoot)
}
