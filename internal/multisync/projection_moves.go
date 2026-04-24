package multisync

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
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

func applyChildProjectionMoves(compiled *compiledMountSet, logger *slog.Logger) bool {
	if compiled == nil || len(compiled.ProjectionMoves) == 0 {
		return false
	}

	changed := false
	failed := make(map[mountID]struct{})
	for i := range compiled.ProjectionMoves {
		move := &compiled.ProjectionMoves[i]
		if _, alreadyFailed := failed[move.mountID]; alreadyFailed {
			continue
		}

		if err := applyChildProjectionMove(move); err != nil {
			if recordProjectionMoveFailure(move, err, logger) {
				changed = true
			}
			compiled.Skipped = append(compiled.Skipped, mountStartupResultForProjectionMove(move, err))
			failed[move.mountID] = struct{}{}
			continue
		}

		if err := recordProjectionMoveSuccess(move); err != nil {
			wrapped := fmt.Errorf("recording child projection move for mount %s: %w", move.mountID, err)
			compiled.Skipped = append(compiled.Skipped, mountStartupResultForProjectionMove(move, wrapped))
			failed[move.mountID] = struct{}{}
			continue
		}
		changed = true
	}

	if len(failed) == 0 {
		return changed
	}

	filtered := compiled.Mounts[:0]
	for _, mount := range compiled.Mounts {
		if _, skip := failed[mount.mountID]; skip {
			continue
		}
		filtered = append(filtered, mount)
	}
	compiled.Mounts = filtered
	return changed
}

func mountStartupResultForProjectionMove(move *childProjectionMove, err error) MountStartupResult {
	return MountStartupResult{
		SelectionIndex: move.selectionIndex,
		Identity:       move.identity,
		DisplayName:    move.displayName,
		Status:         classifyMountStartupError(err),
		Err:            err,
	}
}

func applyChildProjectionMove(move *childProjectionMove) error {
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

func applyChildProjectionMovePaths(move *childProjectionMove, source string, target string) error {
	parentRoot, err := filepath.Abs(filepath.Clean(move.parentSyncRoot))
	if err != nil {
		return projectionUnavailable(move, "resolving parent sync root", err)
	}
	if ancestorErr := validateProjectionAncestors(move, parentRoot, source); ancestorErr != nil {
		return ancestorErr
	}
	if ancestorErr := validateProjectionAncestors(move, parentRoot, target); ancestorErr != nil {
		return ancestorErr
	}

	sourceExists, sourceIsDir, err := projectionPathState(source)
	if err != nil {
		return projectionUnavailable(move, "reading previous local path", err)
	}
	targetExists, targetIsDir, err := projectionPathState(target)
	if err != nil {
		return projectionUnavailable(move, "reading current local path", err)
	}

	return applyProjectionPathDecision(
		move,
		parentRoot,
		source,
		target,
		sourceExists,
		sourceIsDir,
		targetExists,
		targetIsDir,
	)
}

func applyProjectionPathDecision(
	move *childProjectionMove,
	parentRoot string,
	source string,
	target string,
	sourceExists bool,
	sourceIsDir bool,
	targetExists bool,
	targetIsDir bool,
) error {
	switch {
	case sourceExists && !sourceIsDir:
		return projectionConflict(move, "previous local path is not a directory")
	case targetExists && !targetIsDir:
		return projectionConflict(move, "current local path is not a directory")
	case sourceExists && targetExists:
		same, sameErr := projectionPathsSameFile(source, target)
		if sameErr != nil {
			return projectionUnavailable(move, "comparing projection paths", sameErr)
		}
		if same {
			return renameCaseOnlyProjection(move, source, target)
		}
		return projectionConflict(move, "previous and current local paths both exist")
	case !sourceExists && targetExists:
		return nil
	case !sourceExists && !targetExists:
		return projectionUnavailable(move, "moving local projection", fmt.Errorf("previous and current local paths are both missing"))
	default:
		return renameProjectionSource(move, parentRoot, source, target)
	}
}

func projectionConflict(move *childProjectionMove, message string) error {
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

func projectionUnavailable(move *childProjectionMove, action string, err error) error {
	return &childProjectionMoveError{
		state:  config.MountStateUnavailable,
		reason: config.MountStateReasonLocalProjectionUnavailable,
		err:    fmt.Errorf("%s for child mount %s: %w", action, move.mountID, err),
	}
}

func renameProjectionSource(move *childProjectionMove, parentRoot string, source string, target string) error {
	if err := createProjectionParentDirs(move, parentRoot, filepath.Dir(target)); err != nil {
		var moveErr *childProjectionMoveError
		if errors.As(err, &moveErr) {
			return err
		}
		return projectionUnavailable(move, "creating current local path parent", err)
	}
	if err := localpath.Rename(source, target); err != nil {
		return projectionUnavailable(move, "moving local projection", err)
	}

	return nil
}

func renameCaseOnlyProjection(move *childProjectionMove, source string, target string) error {
	if source == target {
		return nil
	}

	tempPath, err := caseOnlyProjectionTempPath(move, target)
	if err != nil {
		return projectionUnavailable(move, "choosing temporary projection rename path", err)
	}
	if err := localpath.Rename(source, tempPath); err != nil {
		return projectionUnavailable(move, "moving local projection to temporary path", err)
	}
	if err := localpath.Rename(tempPath, target); err != nil {
		if rollbackErr := localpath.Rename(tempPath, source); rollbackErr != nil {
			return projectionUnavailable(
				move,
				"moving local projection from temporary path",
				errors.Join(err, fmt.Errorf("rolling back temporary projection rename: %w", rollbackErr)),
			)
		}
		return projectionUnavailable(move, "moving local projection from temporary path", err)
	}

	return nil
}

func caseOnlyProjectionTempPath(move *childProjectionMove, target string) (string, error) {
	sum := sha256.Sum256([]byte(move.mountID.String() + "\x00" + target))
	stem := ".onedrive-go-projection-rename-" + hex.EncodeToString(sum[:])[:16]
	parent := filepath.Dir(target)
	for i := 0; i < 10; i++ {
		name := stem
		if i > 0 {
			name = fmt.Sprintf("%s-%d", stem, i)
		}
		candidate := filepath.Join(parent, name)
		if _, err := localpath.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		} else if err != nil {
			return "", fmt.Errorf("checking temporary projection rename path %s: %w", candidate, err)
		}
	}

	return "", fmt.Errorf("temporary projection rename path already exists for %s", move.mountID)
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

	if info.Mode()&os.ModeSymlink != 0 {
		return true, false, nil
	}

	return true, info.IsDir(), nil
}

func validateProjectionAncestors(move *childProjectionMove, parentRoot string, fullPath string) error {
	targetParent := filepath.Dir(fullPath)
	relativeParent, err := filepath.Rel(parentRoot, targetParent)
	if err != nil {
		return projectionUnavailable(move, "relativizing projection path parent", err)
	}
	if relativeParent == "." {
		return nil
	}
	if relativeParent == parentPathSegment ||
		strings.HasPrefix(relativeParent, parentPathSegment+string(filepath.Separator)) {
		return projectionConflict(move, "projection path escapes parent sync root")
	}

	current := parentRoot
	for _, component := range strings.Split(relativeParent, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, statErr := localpath.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		if statErr != nil {
			return projectionUnavailable(move, "checking projection path ancestor", statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return projectionConflict(move, "projection path ancestor is not a directory")
		}
	}

	return nil
}

func createProjectionParentDirs(move *childProjectionMove, parentRoot string, targetParent string) error {
	relativeParent, err := filepath.Rel(parentRoot, targetParent)
	if err != nil {
		return fmt.Errorf("relativizing current local path parent: %w", err)
	}
	if relativeParent == "." {
		return nil
	}
	if relativeParent == parentPathSegment ||
		strings.HasPrefix(relativeParent, parentPathSegment+string(filepath.Separator)) {
		return projectionConflict(move, "projection path escapes parent sync root")
	}

	current := parentRoot
	for _, component := range strings.Split(relativeParent, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		if err := ensureProjectionParentComponent(move, current); err != nil {
			return err
		}
	}

	return nil
}

func ensureProjectionParentComponent(move *childProjectionMove, current string) error {
	info, statErr := localpath.Lstat(current)
	if statErr == nil {
		return ensureProjectionParentInfo(move, info)
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("checking current local path parent %s: %w", current, statErr)
	}
	if err := localpath.Mkdir(current, childMountRootDirPerms); err != nil {
		if errors.Is(err, os.ErrExist) {
			return validateProjectionParentAfterCreateRace(move, current)
		}
		return fmt.Errorf("creating current local path parent %s: %w", current, err)
	}

	return nil
}

func validateProjectionParentAfterCreateRace(move *childProjectionMove, current string) error {
	info, statErr := localpath.Lstat(current)
	if statErr != nil {
		return fmt.Errorf("checking current local path parent %s after create race: %w", current, statErr)
	}

	return ensureProjectionParentInfo(move, info)
}

func ensureProjectionParentInfo(move *childProjectionMove, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return projectionConflict(move, "projection path ancestor is not a directory")
	}

	return nil
}

func projectionPathsSameFile(source string, target string) (bool, error) {
	sourceInfo, err := localpath.Stat(source)
	if err != nil {
		return false, fmt.Errorf("stat previous local path %s: %w", source, err)
	}
	targetInfo, err := localpath.Stat(target)
	if err != nil {
		return false, fmt.Errorf("stat current local path %s: %w", target, err)
	}

	return os.SameFile(sourceInfo, targetInfo), nil
}

func recordProjectionMoveSuccess(move *childProjectionMove) error {
	if err := config.UpdateMountInventory(func(inventory *config.MountInventory) error {
		record, found := inventory.Mounts[move.mountID.String()]
		if !found {
			return nil
		}

		record.ReservedLocalPaths = removeString(record.ReservedLocalPaths, move.fromRelativeLocalPath)
		if record.StateReason == config.MountStateReasonLocalProjectionUnavailable ||
			record.StateReason == config.MountStateReasonLocalProjectionConflict {
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

func recordProjectionMoveFailure(move *childProjectionMove, err error, logger *slog.Logger) bool {
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
		return false
	}

	return true
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
