package multisync

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

const childMountRootDirPerms os.FileMode = 0o700

func reconcileChildMountLocalRoots(
	parents []*mountSpec,
	inventory *config.MountInventory,
	logger *slog.Logger,
) mountInventoryMutationResult {
	if inventory == nil || len(inventory.Mounts) == 0 {
		return mountInventoryMutationResult{}
	}

	parentsByID := make(map[string]*mountSpec, len(parents))
	for i := range parents {
		if parents[i] == nil {
			continue
		}
		parentsByID[parents[i].mountID.String()] = parents[i]
	}

	result := mountInventoryMutationResult{}
	records := sortedMountRecords(inventory)
	for i := range records {
		record := records[i]
		if !childRootNeedsReconciliation(&record) {
			continue
		}
		parent := parentsByID[record.NamespaceID]
		if parent == nil {
			continue
		}

		if reconcileChildMountRootRecord(inventory, &record, parent, logger) {
			result.changed = true
			result.dirtyMountIDs = appendUniqueStrings(result.dirtyMountIDs, record.MountID)
		}
	}

	return result
}

func reconcileChildMountRootRecord(
	inventory *config.MountInventory,
	record *config.MountRecord,
	parent *mountSpec,
	logger *slog.Logger,
) bool {
	root := filepath.Join(parent.syncRoot, filepath.FromSlash(record.RelativeLocalPath))
	state, reason := materializeChildMountRoot(parent.syncRoot, record.RelativeLocalPath)
	logChildRootLifecycle(record, root, state, reason, logger)

	return setMountLifecycleState(inventory, record, state, reason)
}

func logChildRootLifecycle(
	record *config.MountRecord,
	root string,
	state config.MountState,
	reason string,
	logger *slog.Logger,
) {
	if logger == nil {
		return
	}
	if state == config.MountStateActive && record.State == config.MountStateConflict &&
		record.StateReason == config.MountStateReasonLocalRootCollision {
		logger.Info("child mount local root collision cleared",
			slog.String("mount_id", record.MountID),
			slog.String("path", root),
		)
		return
	}
	if state != config.MountStateActive {
		logger.Warn("child mount local root is not usable",
			slog.String("mount_id", record.MountID),
			slog.String("path", root),
			slog.String("state", string(state)),
			slog.String("reason", reason),
		)
	}
}

func childRootNeedsReconciliation(record *config.MountRecord) bool {
	if record == nil || len(record.ReservedLocalPaths) > 0 {
		return false
	}

	switch record.State {
	case "", config.MountStateActive:
		return true
	case config.MountStateConflict:
		return record.StateReason == config.MountStateReasonLocalRootCollision
	case config.MountStateUnavailable:
		return record.StateReason == config.MountStateReasonLocalRootUnavailable
	case config.MountStatePendingRemoval:
		return false
	default:
		return false
	}
}

func materializeChildMountRoot(parentRoot, relativeLocalPath string) (config.MountState, string) {
	relativePath, ok := cleanChildMountRootRelativePath(relativeLocalPath)
	if !ok {
		return config.MountStateConflict, config.MountStateReasonLocalRootCollision
	}

	root, err := synctree.Open(parentRoot)
	if err != nil {
		return config.MountStateUnavailable, config.MountStateReasonLocalRootUnavailable
	}
	if err := root.MkdirAllNoFollow(relativePath, childMountRootDirPerms); err != nil {
		return childMountRootErrorState(err)
	}

	return config.MountStateActive, ""
}

func cleanChildMountRootRelativePath(relativeLocalPath string) (string, bool) {
	if relativeLocalPath == "" {
		return "", false
	}

	relativePath := filepath.Clean(filepath.FromSlash(relativeLocalPath))
	if filepath.IsAbs(relativePath) || relativePath == "." || relativePath == ".." {
		return "", false
	}
	if strings.HasPrefix(relativePath, ".."+string(os.PathSeparator)) {
		return "", false
	}

	return relativePath, true
}

func childMountRootErrorState(err error) (config.MountState, string) {
	if errors.Is(err, synctree.ErrUnsafePath) {
		return config.MountStateConflict, config.MountStateReasonLocalRootCollision
	}
	if errors.Is(err, syscall.ENOTDIR) {
		return config.MountStateConflict, config.MountStateReasonLocalRootCollision
	}

	return config.MountStateUnavailable, config.MountStateReasonLocalRootUnavailable
}

func validateCompiledChildMountRoots(compiled *compiledMountSet, logger *slog.Logger) {
	if compiled == nil || len(compiled.Mounts) == 0 {
		return
	}

	parentByID := standaloneParentMountsByID(compiled.Mounts)
	filtered := compiled.Mounts[:0]
	for _, mount := range compiled.Mounts {
		if mount == nil || mount.projectionKind != MountProjectionChild {
			filtered = append(filtered, mount)
			continue
		}

		parent := parentByID[mount.parentMountID]
		if parent == nil {
			filtered = append(filtered, mount)
			continue
		}

		state, reason, err := validateCompiledChildMountRoot(parent, mount)
		if err != nil {
			compiled.Skipped = append(compiled.Skipped, mountStartupResultForMount(mount, err))
			recordCompiledChildRootState(mount.mountID, state, reason, logger)
			continue
		}

		filtered = append(filtered, mount)
	}
	compiled.Mounts = filtered
}

func standaloneParentMountsByID(mounts []*mountSpec) map[mountID]*mountSpec {
	parents := make(map[mountID]*mountSpec)
	for i := range mounts {
		if mounts[i] == nil || mounts[i].projectionKind != MountProjectionStandalone {
			continue
		}
		parents[mounts[i].mountID] = mounts[i]
	}

	return parents
}

func validateCompiledChildMountRoot(
	parent *mountSpec,
	child *mountSpec,
) (config.MountState, string, error) {
	relativeLocalPath, err := childMountRelativePath(parent, child)
	if err != nil {
		return config.MountStateUnavailable,
			config.MountStateReasonLocalRootUnavailable,
			fmt.Errorf("child mount %s local root: %w", child.mountID, err)
	}

	state, reason := materializeChildMountRoot(parent.syncRoot, relativeLocalPath)
	if state == config.MountStateActive {
		return state, reason, nil
	}

	return state, reason, childRootStateError(child.mountID, state, reason)
}

func childMountRelativePath(parent *mountSpec, child *mountSpec) (string, error) {
	parentRoot, err := filepath.Abs(filepath.Clean(parent.syncRoot))
	if err != nil {
		return "", fmt.Errorf("resolving parent sync root %q: %w", parent.syncRoot, err)
	}
	childRoot, err := filepath.Abs(filepath.Clean(child.syncRoot))
	if err != nil {
		return "", fmt.Errorf("resolving child sync root %q: %w", child.syncRoot, err)
	}

	relativePath, err := filepath.Rel(parentRoot, childRoot)
	if err != nil {
		return "", fmt.Errorf("relativizing child sync root %q: %w", child.syncRoot, err)
	}
	relativePath = filepath.ToSlash(relativePath)
	if _, ok := cleanChildMountRootRelativePath(relativePath); !ok {
		return "", fmt.Errorf("path %q escapes parent sync root", relativePath)
	}

	return relativePath, nil
}

func childRootStateError(mountID mountID, state config.MountState, reason string) error {
	switch state {
	case config.MountStateActive:
		return nil
	case config.MountStateConflict:
		if reason != "" {
			return fmt.Errorf("child mount %s is conflicted: %s", mountID, reason)
		}
		return fmt.Errorf("child mount %s is conflicted", mountID)
	case config.MountStatePendingRemoval:
		return fmt.Errorf("child mount %s is pending removal", mountID)
	case config.MountStateUnavailable:
		if reason != "" {
			return fmt.Errorf("child mount %s is unavailable: %s", mountID, reason)
		}
		return fmt.Errorf("child mount %s is unavailable", mountID)
	default:
		return fmt.Errorf("child mount %s local root has unsupported state %q", mountID, state)
	}
}

func recordCompiledChildRootState(
	childMountID mountID,
	state config.MountState,
	reason string,
	logger *slog.Logger,
) {
	if err := config.UpdateMountInventory(func(inventory *config.MountInventory) error {
		record, found := inventory.Mounts[childMountID.String()]
		if !found {
			return nil
		}
		setMountLifecycleState(inventory, &record, state, reason)
		return nil
	}); err != nil && logger != nil {
		logger.Warn("recording child mount local root failure",
			slog.String("mount_id", childMountID.String()),
			slog.String("error", err.Error()),
		)
	}
}
