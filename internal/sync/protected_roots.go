package sync

import (
	"io/fs"
	"path"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

func NormalizedProtectedRootPath(value string) string {
	normalized := filepath.ToSlash(value)
	normalized = path.Clean("/" + normalized)
	normalized = normalized[1:]
	if normalized == "." {
		return ""
	}

	return normalized
}

func ProtectedRootPathReservation(observedPath string, protectedRoots []ProtectedRoot) (ProtectedRoot, bool) {
	normalized := NormalizedProtectedRootPath(observedPath)
	if normalized == "" {
		return ProtectedRoot{}, false
	}

	for _, protectedRoot := range protectedRoots {
		reservedPath := NormalizedProtectedRootPath(protectedRoot.Path)
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

func ProtectedRootIdentityReservation(
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

	normalized := NormalizedProtectedRootPath(observedPath)
	if normalized == "" {
		return ProtectedRoot{}, false
	}
	parent := path.Dir(normalized)
	for _, protectedRoot := range protectedRoots {
		if !protectedRoot.HasIdentity ||
			!synctree.SameIdentity(
				protectedRoot.FileIdentity(),
				identity,
			) {
			continue
		}
		reservedPath := NormalizedProtectedRootPath(protectedRoot.Path)
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
