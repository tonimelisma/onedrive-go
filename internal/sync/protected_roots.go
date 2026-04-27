package sync

import (
	"context"
	"io/fs"
	"path"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

func (e *Engine) refreshProtectedRootsFromStore(ctx context.Context) error {
	if e == nil || e.baseline == nil {
		return nil
	}
	if e.shortcutNamespaceID == "" {
		e.protectedRoots = nil
		return nil
	}
	records, err := e.baseline.ListShortcutRoots(ctx)
	if err != nil {
		return err
	}
	e.protectedRoots = protectedRootsForShortcutRoots(records, e.shortcutNamespaceID)
	return nil
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
				synctree.FileIdentity{Device: protectedRoot.Device, Inode: protectedRoot.Inode},
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
