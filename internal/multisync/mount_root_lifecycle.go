package multisync

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

const childMountRootDirPerms os.FileMode = 0o700

func reconcileChildMountLocalRoots(
	parents []*mountSpec,
	inventory *config.MountInventory,
	logger *slog.Logger,
) bool {
	if inventory == nil || len(inventory.Mounts) == 0 {
		return false
	}

	parentsByID := make(map[string]*mountSpec, len(parents))
	for i := range parents {
		if parents[i] == nil {
			continue
		}
		parentsByID[parents[i].mountID.String()] = parents[i]
	}

	changed := false
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

		changed = reconcileChildMountRootRecord(inventory, &record, parent, logger) || changed
	}

	return changed
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

	state, reason := existingDirectoryState(parentRoot)
	if state != config.MountStateActive {
		return state, reason
	}

	current := parentRoot
	for _, component := range strings.Split(relativePath, string(os.PathSeparator)) {
		current = filepath.Join(current, component)
		state, reason := ensureChildMountRootComponent(current)
		if state != config.MountStateActive {
			return state, reason
		}
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

func ensureChildMountRootComponent(root string) (config.MountState, string) {
	state, reason := existingDirectoryState(root)
	if state == config.MountStateActive {
		return state, reason
	}
	if state != config.MountStateUnavailable || reason != config.MountStateReasonLocalRootUnavailable {
		return state, reason
	}

	if err := localpath.Mkdir(root, childMountRootDirPerms); err != nil {
		if errors.Is(err, os.ErrExist) {
			return existingDirectoryState(root)
		}
		return childMountRootCreateErrorState(err)
	}

	return existingDirectoryState(root)
}

func existingDirectoryState(root string) (config.MountState, string) {
	info, err := localpath.Lstat(root)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return config.MountStateConflict, config.MountStateReasonLocalRootCollision
		}
		return config.MountStateActive, ""
	}
	if errors.Is(err, syscall.ENOTDIR) {
		return config.MountStateConflict, config.MountStateReasonLocalRootCollision
	}
	if !errors.Is(err, os.ErrNotExist) {
		return config.MountStateUnavailable, config.MountStateReasonLocalRootUnavailable
	}

	return config.MountStateUnavailable, config.MountStateReasonLocalRootUnavailable
}

func childMountRootCreateErrorState(err error) (config.MountState, string) {
	if errors.Is(err, syscall.ENOTDIR) {
		return config.MountStateConflict, config.MountStateReasonLocalRootCollision
	}

	return config.MountStateUnavailable, config.MountStateReasonLocalRootUnavailable
}
