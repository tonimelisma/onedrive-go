package sync

import (
	"io/fs"
	"path"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

func normalizedManagedRootPath(value string) string {
	normalized := filepath.ToSlash(value)
	normalized = path.Clean("/" + normalized)
	normalized = normalized[1:]
	if normalized == "." {
		return ""
	}

	return normalized
}

func managedRootPathReservation(observedPath string, reservations []ManagedRootReservation) (ManagedRootReservation, bool) {
	normalized := normalizedManagedRootPath(observedPath)
	if normalized == "" {
		return ManagedRootReservation{}, false
	}

	for _, reservation := range reservations {
		reservedPath := normalizedManagedRootPath(reservation.Path)
		if reservedPath == "" {
			continue
		}
		if normalized == reservedPath || pathHasPrefix(normalized, reservedPath) {
			reservation.Path = reservedPath
			return reservation, true
		}
	}

	return ManagedRootReservation{}, false
}

func managedRootIdentityReservation(
	observedPath string,
	info fs.FileInfo,
	reservations []ManagedRootReservation,
) (ManagedRootReservation, bool) {
	if info == nil || !info.IsDir() {
		return ManagedRootReservation{}, false
	}
	identity, ok := synctree.IdentityFromFileInfo(info)
	if !ok {
		return ManagedRootReservation{}, false
	}

	normalized := normalizedManagedRootPath(observedPath)
	if normalized == "" {
		return ManagedRootReservation{}, false
	}
	parent := path.Dir(normalized)
	for _, reservation := range reservations {
		if !reservation.HasIdentity ||
			!synctree.SameIdentity(
				synctree.FileIdentity{Device: reservation.Device, Inode: reservation.Inode},
				identity,
			) {
			continue
		}
		reservedPath := normalizedManagedRootPath(reservation.Path)
		if reservedPath == "" || reservedPath == normalized {
			continue
		}
		if path.Dir(reservedPath) != parent {
			continue
		}
		reservation.Path = reservedPath
		return reservation, true
	}

	return ManagedRootReservation{}, false
}

func pathHasPrefix(observedPath string, parentPath string) bool {
	return len(observedPath) > len(parentPath) &&
		observedPath[:len(parentPath)] == parentPath &&
		observedPath[len(parentPath)] == '/'
}
