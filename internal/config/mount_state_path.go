package config

import (
	"crypto/sha256"
	"encoding/base64"
	"path/filepath"
	"strings"
)

const stateMountPrefix = "state_mount_"

func MountStatePath(mountID string) string {
	return MountStatePathForDataDir(DefaultDataDir(), mountID)
}

func MountStatePathForDataDir(dataDir string, mountID string) string {
	ref := MountRef(mountID)
	if dataDir == "" || ref == "" {
		return ""
	}

	return filepath.Join(dataDir, stateMountPrefix+ref+".db")
}

// MountRef returns a stable, opaque reference for a mount.
//
// It exists so machine-readable surfaces can identify a specific mount across
// runs without publishing the mount ID itself. A mount ID embeds the account
// email and, for shortcut children, the remote drive and item IDs; R-3.1.6
// keeps those out of public status JSON deliberately. The digest is stable for
// a given mount, unique between mounts, and reveals none of its inputs.
//
// It is the same digest that names a child mount's state database, so a
// support conversation can map a status row to its state file directly.
func MountRef(mountID string) string {
	if strings.TrimSpace(mountID) == "" {
		return ""
	}

	// Hash the mount ID exactly as given. This digest names a child mount's
	// state database on disk, so normalizing the input here would rename
	// existing state files and orphan their contents.
	sum := sha256.Sum256([]byte(mountID))

	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func ChildMountID(parentMountID, bindingItemID string) string {
	parent := strings.TrimSpace(parentMountID)
	binding := strings.TrimSpace(bindingItemID)
	if parent == "" || binding == "" {
		return ""
	}

	return parent + "|binding:" + binding
}

func IsChildMountID(mountID string) bool {
	return strings.Contains(strings.TrimSpace(mountID), "|binding:")
}
