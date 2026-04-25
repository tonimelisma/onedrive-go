package sync

import (
	"io/fs"
	"path"
	"path/filepath"
	"syscall"
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
	device, inode, ok := fileInfoIdentity(info)
	if !ok {
		return ManagedRootReservation{}, false
	}

	normalized := normalizedManagedRootPath(observedPath)
	if normalized == "" {
		return ManagedRootReservation{}, false
	}
	parent := path.Dir(normalized)
	for _, reservation := range reservations {
		if !reservation.HasIdentity || reservation.Device != device || reservation.Inode != inode {
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

func fileInfoIdentity(info fs.FileInfo) (uint64, uint64, bool) {
	if info == nil {
		return 0, 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return 0, 0, false
	}

	device := statDeviceID(stat)
	inode := stat.Ino
	if device == 0 && inode == 0 {
		return 0, 0, false
	}

	return device, inode, true
}

func pathHasPrefix(observedPath string, parentPath string) bool {
	return len(observedPath) > len(parentPath) &&
		observedPath[:len(parentPath)] == parentPath &&
		observedPath[len(parentPath)] == '/'
}
