package config

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
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

// PurgeManagedChildMountArtifacts deletes managed child-owned durable artifacts.
// It intentionally refuses IDs that do not have the automatic child-mount shape
// so explicit user-configured drive catalog entries are not removed by release
// cleanup.
func PurgeManagedChildMountArtifacts(childMountID string) error {
	if !IsChildMountID(childMountID) {
		return nil
	}

	var errs []error
	for _, path := range managedChildMountStateArtifactPaths(childMountID) {
		if path == "" {
			continue
		}
		if err := localpath.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove child state artifact %s: %w", path, err))
			continue
		}
	}
	if err := pruneManagedChildCatalogRecord(childMountID); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func managedChildMountStateArtifactPaths(childMountID string) []string {
	statePath := MountStatePath(childMountID)
	if statePath == "" {
		return nil
	}
	return []string{
		statePath,
		statePath + "-wal",
		statePath + "-shm",
		statePath + "-journal",
	}
}

func pruneManagedChildCatalogRecord(childMountID string) error {
	if !IsChildMountID(childMountID) {
		return nil
	}
	catalogPath := CatalogPath()
	if catalogPath == "" {
		return nil
	}
	if _, err := localpath.Stat(catalogPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat catalog before child cleanup: %w", err)
	}

	catalog, err := LoadCatalog()
	if err != nil {
		return fmt.Errorf("load catalog before child cleanup: %w", err)
	}
	if _, found := catalog.Drives[childMountID]; !found {
		return nil
	}

	if err := UpdateCatalog(func(catalog *Catalog) error {
		delete(catalog.Drives, childMountID)
		return nil
	}); err != nil {
		return fmt.Errorf("update catalog for child cleanup: %w", err)
	}
	return nil
}
