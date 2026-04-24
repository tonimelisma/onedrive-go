package multisync

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

const (
	parentPathSegment = ".."
)

type childProjectionMoveError struct {
	state  config.MountState
	reason string
	err    error
}

func (f *childProjectionMoveError) Error() string {
	if f == nil || f.err == nil {
		return ""
	}
	if f.reason != "" {
		return fmt.Sprintf("%s: %s", f.reason, f.err.Error())
	}

	return f.err.Error()
}

func (f *childProjectionMoveError) Unwrap() error {
	if f == nil {
		return nil
	}

	return f.err
}

func applyChildProjectionMoves(compiled *compiledMountSet, logger *slog.Logger) {
	if compiled == nil || len(compiled.ProjectionMoves) == 0 {
		return
	}

	mountByID := mountSpecByID(compiled.Mounts)
	failed := make(map[mountID]struct{})
	for _, move := range compiled.ProjectionMoves {
		mount := mountByID[move.mountID]
		if mount == nil {
			continue
		}
		if _, alreadyFailed := failed[move.mountID]; alreadyFailed {
			continue
		}

		if err := applyChildProjectionMove(move); err != nil {
			recordProjectionMoveFailure(move, err, logger)
			compiled.Skipped = append(compiled.Skipped, mountStartupResultForMount(mount, err))
			failed[move.mountID] = struct{}{}
			continue
		}

		if err := recordProjectionMoveSuccess(move); err != nil {
			wrapped := fmt.Errorf("recording child projection move for mount %s: %w", move.mountID, err)
			compiled.Skipped = append(compiled.Skipped, mountStartupResultForMount(mount, wrapped))
			failed[move.mountID] = struct{}{}
		}
	}

	if len(failed) == 0 {
		return
	}

	filtered := compiled.Mounts[:0]
	for _, mount := range compiled.Mounts {
		if _, skip := failed[mount.mountID]; skip {
			continue
		}
		filtered = append(filtered, mount)
	}
	compiled.Mounts = filtered
}

func mountSpecByID(mounts []*mountSpec) map[mountID]*mountSpec {
	byID := make(map[mountID]*mountSpec, len(mounts))
	for i := range mounts {
		byID[mounts[i].mountID] = mounts[i]
	}

	return byID
}

func applyChildProjectionMove(move childProjectionMove) error {
	source, err := childProjectionAbsolutePath(move.parentSyncRoot, move.fromRelativeLocalPath)
	if err != nil {
		return projectionUnavailable(move, "resolving previous local path", err)
	}
	target, err := childProjectionAbsolutePath(move.parentSyncRoot, move.toRelativeLocalPath)
	if err != nil {
		return projectionUnavailable(move, "resolving current local path", err)
	}
	if source == target {
		return nil
	}

	return applyChildProjectionMovePaths(move, source, target)
}

func applyChildProjectionMovePaths(move childProjectionMove, source string, target string) error {
	sourceExists, sourceIsDir, err := projectionPathState(source)
	if err != nil {
		return projectionUnavailable(move, "reading previous local path", err)
	}
	targetExists, targetIsDir, err := projectionPathState(target)
	if err != nil {
		return projectionUnavailable(move, "reading current local path", err)
	}

	switch {
	case sourceExists && !sourceIsDir:
		return projectionConflict(move, "previous local path is not a directory")
	case targetExists && !targetIsDir:
		return projectionConflict(move, "current local path is not a directory")
	case sourceExists && targetExists:
		return projectionConflict(move, "previous and current local paths both exist")
	case !sourceExists && targetExists:
		return nil
	case !sourceExists && !targetExists:
		return createProjectionTarget(move, target)
	default:
		return renameProjectionSource(move, source, target)
	}
}

func projectionConflict(move childProjectionMove, message string) error {
	return &childProjectionMoveError{
		state:  config.MountStateConflict,
		reason: config.MountStateReasonLocalProjectionConflict,
		err: fmt.Errorf(
			"child mount %s is conflicted: %s: %s (%s -> %s)",
			move.mountID,
			config.MountStateReasonLocalProjectionConflict,
			message,
			move.fromRelativeLocalPath,
			move.toRelativeLocalPath,
		),
	}
}

func projectionUnavailable(move childProjectionMove, action string, err error) error {
	return &childProjectionMoveError{
		state:  config.MountStateUnavailable,
		reason: config.MountStateReasonLocalProjectionUnavailable,
		err:    fmt.Errorf("%s for child mount %s: %w", action, move.mountID, err),
	}
}

func createProjectionTarget(move childProjectionMove, _ string) error {
	return materializeProjectionPath(move, "creating current local path", move.toRelativeLocalPath)
}

func renameProjectionSource(move childProjectionMove, source string, target string) error {
	if err := materializeProjectionParent(move); err != nil {
		return err
	}
	if err := localpath.Rename(source, target); err != nil {
		return projectionUnavailable(move, "moving local projection", err)
	}

	return nil
}

func materializeProjectionParent(move childProjectionMove) error {
	parentRel := path.Dir(move.toRelativeLocalPath)
	if parentRel == "." || parentRel == "/" {
		state, reason := existingDirectoryState(move.parentSyncRoot)
		if state == config.MountStateActive {
			return nil
		}
		return projectionMaterializeError(move, "validating current local path parent", state, reason)
	}

	return materializeProjectionPath(move, "creating current local path parent", parentRel)
}

func materializeProjectionPath(move childProjectionMove, action string, rel string) error {
	state, reason := materializeChildMountRoot(move.parentSyncRoot, rel)
	if state == config.MountStateActive {
		return nil
	}

	return projectionMaterializeError(move, action, state, reason)
}

func projectionMaterializeError(
	move childProjectionMove,
	action string,
	state config.MountState,
	reason string,
) error {
	switch state {
	case config.MountStateActive:
		return nil
	case config.MountStateConflict:
		return &childProjectionMoveError{
			state:  config.MountStateConflict,
			reason: config.MountStateReasonLocalProjectionConflict,
			err: fmt.Errorf(
				"%s for child mount %s: %s",
				action,
				move.mountID,
				reason,
			),
		}
	case config.MountStatePendingRemoval:
		return projectionUnavailable(
			move,
			action,
			fmt.Errorf("unexpected pending-removal local projection"),
		)
	case config.MountStateUnavailable:
		return &childProjectionMoveError{
			state:  config.MountStateUnavailable,
			reason: config.MountStateReasonLocalProjectionUnavailable,
			err: fmt.Errorf(
				"%s for child mount %s: %s",
				action,
				move.mountID,
				reason,
			),
		}
	default:
		return projectionUnavailable(
			move,
			action,
			fmt.Errorf("unsupported local projection state %q", state),
		)
	}
}

func childProjectionAbsolutePath(parentSyncRoot string, rel string) (string, error) {
	parentRoot, err := filepath.Abs(filepath.Clean(parentSyncRoot))
	if err != nil {
		return "", fmt.Errorf("resolving parent sync root %q: %w", parentSyncRoot, err)
	}
	cleanRel := filepath.Clean(filepath.FromSlash(rel))
	if cleanRel == "." || cleanRel == "" || filepath.IsAbs(cleanRel) {
		return "", fmt.Errorf("path %q must be relative", rel)
	}
	if cleanRel == parentPathSegment ||
		strings.HasPrefix(cleanRel, parentPathSegment+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes parent sync root", rel)
	}

	fullPath := filepath.Join(parentRoot, cleanRel)
	relativeToRoot, err := filepath.Rel(parentRoot, fullPath)
	if err != nil {
		return "", fmt.Errorf("relativizing child projection path %q: %w", rel, err)
	}
	if relativeToRoot == parentPathSegment ||
		strings.HasPrefix(relativeToRoot, parentPathSegment+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes parent sync root", rel)
	}

	return fullPath, nil
}

func projectionPathState(path string) (exists bool, isDir bool, err error) {
	info, err := localpath.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("reading projection path %s: %w", path, err)
	}

	return true, info.IsDir(), nil
}

func recordProjectionMoveSuccess(move childProjectionMove) error {
	if err := config.UpdateMountInventory(func(inventory *config.MountInventory) error {
		record, found := inventory.Mounts[move.mountID.String()]
		if !found {
			return nil
		}

		record.ReservedLocalPaths = removeString(record.ReservedLocalPaths, move.fromRelativeLocalPath)
		if record.State == config.MountStateUnavailable &&
			record.StateReason == config.MountStateReasonLocalProjectionUnavailable {
			record.State = config.MountStateActive
			record.StateReason = ""
		}
		inventory.Mounts[record.MountID] = record
		return nil
	}); err != nil {
		return fmt.Errorf("updating mount inventory after projection move: %w", err)
	}

	return nil
}

func recordProjectionMoveFailure(move childProjectionMove, err error, logger *slog.Logger) {
	var failure *childProjectionMoveError
	if !errors.As(err, &failure) {
		failure = &childProjectionMoveError{
			state:  config.MountStateUnavailable,
			reason: config.MountStateReasonLocalProjectionUnavailable,
			err:    err,
		}
	}

	if updateErr := config.UpdateMountInventory(func(inventory *config.MountInventory) error {
		record, found := inventory.Mounts[move.mountID.String()]
		if !found {
			return nil
		}
		record.State = failure.state
		record.StateReason = failure.reason
		record.ReservedLocalPaths = appendUniqueStrings(record.ReservedLocalPaths, move.fromRelativeLocalPath)
		inventory.Mounts[record.MountID] = record
		return nil
	}); updateErr != nil && logger != nil {
		logger.Warn("recording child projection move failure",
			slog.String("mount_id", move.mountID.String()),
			slog.String("error", updateErr.Error()),
		)
	}
}

func removeString(values []string, remove string) []string {
	if len(values) == 0 {
		return nil
	}

	filtered := values[:0]
	for _, value := range values {
		if value == remove {
			continue
		}
		filtered = append(filtered, value)
	}
	if len(filtered) == 0 {
		return nil
	}

	return filtered
}
