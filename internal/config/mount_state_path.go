package config

import (
	"crypto/sha256"
	"encoding/base64"
	"path/filepath"
	"strings"
)

const stateMountPrefix = "state_mount_"

func MountStatePath(mountID string) string {
	dataDir := DefaultDataDir()
	if dataDir == "" || strings.TrimSpace(mountID) == "" {
		return ""
	}

	sum := sha256.Sum256([]byte(mountID))
	encodedDigest := base64.RawURLEncoding.EncodeToString(sum[:])
	return filepath.Join(dataDir, stateMountPrefix+encodedDigest+".db")
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
