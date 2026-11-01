package sync

import (
	"context"
	"io/fs"
	"path"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// refreshProtectedRootsFromStore rederives the protected roots from the
// persisted shortcut-root state. It runs on the loop goroutine before every
// observation and off it from the full-refresh goroutine and the remote
// observer's expired-delta recovery, so the write goes through the lock.
func (e *Engine) refreshProtectedRootsFromStore(ctx context.Context) error {
	if e == nil || e.baseline == nil {
		return nil
	}
	if e.shortcutNamespaceID == "" {
		e.setProtectedRoots(nil)
		return nil
	}
	records, err := e.baseline.listShortcutRoots(ctx)
	if err != nil {
		return err
	}
	e.setProtectedRoots(protectedRootsForShortcutRoots(records, e.shortcutNamespaceID))
	return nil
}

// setProtectedRoots replaces the protected roots under protectedRootsMu.
func (e *Engine) setProtectedRoots(roots []ProtectedRoot) {
	e.protectedRootsMu.Lock()
	defer e.protectedRootsMu.Unlock()
	e.protectedRoots = roots
}

// currentProtectedRoots returns a copy of the protected roots, so a reader
// keeps a stable list while a concurrent refresh replaces the engine's.
func (e *Engine) currentProtectedRoots() []ProtectedRoot {
	e.protectedRootsMu.Lock()
	defer e.protectedRootsMu.Unlock()
	if e.protectedRoots == nil {
		return nil
	}

	return append([]ProtectedRoot(nil), e.protectedRoots...)
}

func normalizedProtectedRootPath(value string) string {
	normalized := filepath.ToSlash(value)
	normalized = path.Clean("/" + normalized)
	normalized = normalized[1:]
	if normalized == "." {
		return ""
	}

	return normalized
}

func protectedRootPathReservation(observedPath string, protectedRoots []ProtectedRoot) (ProtectedRoot, bool) {
	normalized := normalizedProtectedRootPath(observedPath)
	if normalized == "" {
		return ProtectedRoot{}, false
	}

	for _, protectedRoot := range protectedRoots {
		reservedPath := normalizedProtectedRootPath(protectedRoot.Path)
		if reservedPath == "" {
			continue
		}
		if normalized == reservedPath || pathHasPrefix(normalized, reservedPath) {
			protectedRoot.Path = reservedPath
			return protectedRoot, true
		}
	}

	return ProtectedRoot{}, false
}

func protectedRootIdentityReservation(
	observedPath string,
	info fs.FileInfo,
	protectedRoots []ProtectedRoot,
) (ProtectedRoot, bool) {
	if info == nil || !info.IsDir() {
		return ProtectedRoot{}, false
	}
	identity, ok := synctree.IdentityFromFileInfo(info)
	if !ok {
		return ProtectedRoot{}, false
	}

	normalized := normalizedProtectedRootPath(observedPath)
	if normalized == "" {
		return ProtectedRoot{}, false
	}
	parent := path.Dir(normalized)
	for _, protectedRoot := range protectedRoots {
		if !protectedRoot.HasIdentity ||
			!synctree.SameIdentity(
				protectedRoot.fileIdentity(),
				identity,
			) {
			continue
		}
		reservedPath := normalizedProtectedRootPath(protectedRoot.Path)
		if reservedPath == "" || reservedPath == normalized {
			continue
		}
		if path.Dir(reservedPath) != parent {
			continue
		}
		protectedRoot.Path = reservedPath
		return protectedRoot, true
	}

	return ProtectedRoot{}, false
}

func pathHasPrefix(observedPath string, parentPath string) bool {
	return len(observedPath) > len(parentPath) &&
		observedPath[:len(parentPath)] == parentPath &&
		observedPath[len(parentPath)] == '/'
}
