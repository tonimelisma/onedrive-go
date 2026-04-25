package multisync

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

const (
	parentPathSegment                = ".."
	caseOnlyProjectionRenameAttempts = 10
)

type childProjectionMoveError struct {
	state  config.MountState
	reason config.MountStateReason
	err    error
}

type projectionPathDecisionKind string

const (
	projectionDecisionNoop                 projectionPathDecisionKind = "noop"
	projectionDecisionRenameSource         projectionPathDecisionKind = "rename_source"
	projectionDecisionInspectExistingPaths projectionPathDecisionKind = "inspect_existing_paths"
	projectionDecisionRenameCaseOnly       projectionPathDecisionKind = "rename_case_only"
	projectionDecisionReplaceEmptyTarget   projectionPathDecisionKind = "replace_empty_target"
	projectionDecisionRemoveMatchingSource projectionPathDecisionKind = "remove_matching_source"
	projectionDecisionConflict             projectionPathDecisionKind = "conflict"
	projectionDecisionUnavailable          projectionPathDecisionKind = "unavailable"
)

type projectionPathDecision struct {
	kind              projectionPathDecisionKind
	conflictMessage   string
	unavailableAction string
	unavailableErr    error
}

type projectionExistingPathIssue string

const (
	projectionExistingPathIssueUnsupported projectionExistingPathIssue = "unsupported"
	projectionExistingPathIssueUnavailable projectionExistingPathIssue = "unavailable"
)

type projectionExistingPathInspection struct {
	sameFile          bool
	targetEmpty       bool
	treesEqual        bool
	issue             projectionExistingPathIssue
	unavailableAction string
	unavailableErr    error
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
	root, err := synctree.Open(move.parentSyncRoot)
	if err != nil {
		return projectionUnavailable(move, "opening parent sync root", err)
	}
	sourceRel, err := childProjectionRelativePath(move.fromRelativeLocalPath)
	if err != nil {
		return projectionUnavailable(move, "resolving previous local path", err)
	}
	targetRel, err := childProjectionRelativePath(move.toRelativeLocalPath)
	if err != nil {
		return projectionUnavailable(move, "resolving current local path", err)
	}
	if sourceRel == targetRel {
		return nil
	}

	return applyChildProjectionMovePaths(move, root, sourceRel, targetRel)
}

func applyChildProjectionMovePaths(
	move *childProjectionMove,
	root *synctree.Root,
	sourceRel string,
	targetRel string,
) error {
	if ancestorErr := validateProjectionAncestors(move, root, sourceRel); ancestorErr != nil {
		return ancestorErr
	}
	if ancestorErr := validateProjectionAncestors(move, root, targetRel); ancestorErr != nil {
		return ancestorErr
	}

	sourceState, err := projectionPathState(root, sourceRel)
	if err != nil {
		return projectionUnavailable(move, "reading previous local path", err)
	}
	targetState, err := projectionPathState(root, targetRel)
	if err != nil {
		return projectionUnavailable(move, "reading current local path", err)
	}

	return applyProjectionPathDecision(
		move,
		root,
		sourceRel,
		targetRel,
		sourceState,
		targetState,
	)
}

func applyProjectionPathDecision(
	move *childProjectionMove,
	root *synctree.Root,
	sourceRel string,
	targetRel string,
	sourceState synctree.PathState,
	targetState synctree.PathState,
) error {
	decision := classifyProjectionPathStates(sourceState, targetState)
	if decision.kind == projectionDecisionInspectExistingPaths {
		inspection := inspectExistingProjectionPaths(root, sourceRel, targetRel)
		decision = classifyExistingProjectionPathDecision(inspection)
	}

	return executeProjectionPathDecision(move, root, sourceRel, targetRel, decision)
}

func classifyProjectionPathStates(sourceState synctree.PathState, targetState synctree.PathState) projectionPathDecision {
	switch {
	case sourceState.Exists && !sourceState.IsDir:
		return projectionPathDecision{kind: projectionDecisionConflict, conflictMessage: "previous local path is not a directory"}
	case targetState.Exists && !targetState.IsDir:
		return projectionPathDecision{kind: projectionDecisionConflict, conflictMessage: "current local path is not a directory"}
	case sourceState.Exists && targetState.Exists:
		return projectionPathDecision{kind: projectionDecisionInspectExistingPaths}
	case !sourceState.Exists && targetState.Exists:
		return projectionPathDecision{kind: projectionDecisionNoop}
	case !sourceState.Exists && !targetState.Exists:
		return projectionPathDecision{
			kind:              projectionDecisionUnavailable,
			unavailableAction: "moving local projection",
			unavailableErr:    fmt.Errorf("previous and current local paths are both missing"),
		}
	default:
		return projectionPathDecision{kind: projectionDecisionRenameSource}
	}
}

func inspectExistingProjectionPaths(root *synctree.Root, sourceRel string, targetRel string) projectionExistingPathInspection {
	same, sameErr := projectionPathsSameFile(root, sourceRel, targetRel)
	if sameErr != nil {
		return projectionExistingPathInspection{
			issue:             projectionExistingPathIssueUnavailable,
			unavailableAction: "comparing projection paths",
			unavailableErr:    sameErr,
		}
	}
	if same {
		return projectionExistingPathInspection{sameFile: true}
	}

	targetEmpty, emptyErr := root.DirEmptyNoFollow(targetRel)
	if emptyErr != nil {
		return projectionExistingPathInspectionError("checking current local path", emptyErr)
	}
	if targetEmpty {
		if validateErr := root.ValidateTreeNoFollow(sourceRel); validateErr != nil {
			return projectionExistingPathInspectionError("checking previous local path", validateErr)
		}
		return projectionExistingPathInspection{targetEmpty: true}
	}

	matches, matchErr := root.TreesEqualNoFollow(sourceRel, targetRel)
	if matchErr != nil {
		return projectionExistingPathInspectionError("comparing projection trees", matchErr)
	}

	return projectionExistingPathInspection{treesEqual: matches}
}

func projectionExistingPathInspectionError(action string, err error) projectionExistingPathInspection {
	if errors.Is(err, synctree.ErrUnsafePath) || errors.Is(err, synctree.ErrUnsupportedTreeEntry) {
		return projectionExistingPathInspection{issue: projectionExistingPathIssueUnsupported}
	}

	return projectionExistingPathInspection{
		issue:             projectionExistingPathIssueUnavailable,
		unavailableAction: action,
		unavailableErr:    err,
	}
}

func classifyExistingProjectionPathDecision(inspection projectionExistingPathInspection) projectionPathDecision {
	switch {
	case inspection.issue == projectionExistingPathIssueUnsupported:
		return projectionPathDecision{kind: projectionDecisionConflict, conflictMessage: "projection tree contains unsupported local content"}
	case inspection.issue == projectionExistingPathIssueUnavailable:
		return projectionPathDecision{
			kind:              projectionDecisionUnavailable,
			unavailableAction: inspection.unavailableAction,
			unavailableErr:    inspection.unavailableErr,
		}
	case inspection.sameFile:
		return projectionPathDecision{kind: projectionDecisionRenameCaseOnly}
	case inspection.targetEmpty:
		return projectionPathDecision{kind: projectionDecisionReplaceEmptyTarget}
	case inspection.treesEqual:
		return projectionPathDecision{kind: projectionDecisionRemoveMatchingSource}
	default:
		return projectionPathDecision{kind: projectionDecisionConflict, conflictMessage: "previous and current local paths both exist"}
	}
}

func executeProjectionPathDecision(
	move *childProjectionMove,
	root *synctree.Root,
	sourceRel string,
	targetRel string,
	decision projectionPathDecision,
) error {
	switch decision.kind {
	case projectionDecisionNoop:
		return nil
	case projectionDecisionRenameSource:
		return renameProjectionSource(move, root, sourceRel, targetRel)
	case projectionDecisionRenameCaseOnly:
		return renameCaseOnlyProjection(move, root, sourceRel, targetRel)
	case projectionDecisionReplaceEmptyTarget:
		if err := root.RemoveTreeNoFollow(targetRel); err != nil {
			return projectionTreeMutationError(move, "removing empty current local path", err)
		}
		return renameProjectionSource(move, root, sourceRel, targetRel)
	case projectionDecisionRemoveMatchingSource:
		if err := root.RemoveTreeNoFollow(sourceRel); err != nil {
			return projectionTreeMutationError(move, "removing previous matching local path", err)
		}
		return nil
	case projectionDecisionConflict:
		return projectionConflict(move, decision.conflictMessage)
	case projectionDecisionUnavailable:
		return projectionUnavailable(move, decision.unavailableAction, decision.unavailableErr)
	case projectionDecisionInspectExistingPaths:
		return projectionUnavailable(
			move,
			"classifying local projection move",
			fmt.Errorf("existing projection path inspection was not resolved"),
		)
	default:
		return projectionUnavailable(move, "classifying local projection move", fmt.Errorf("unknown projection path decision %q", decision.kind))
	}
}

func projectionTreeMutationError(move *childProjectionMove, action string, err error) error {
	if errors.Is(err, synctree.ErrUnsafePath) || errors.Is(err, synctree.ErrUnsupportedTreeEntry) {
		return projectionConflict(move, "projection tree contains unsupported local content")
	}

	return projectionUnavailable(move, action, err)
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

func renameProjectionSource(
	move *childProjectionMove,
	root *synctree.Root,
	sourceRel string,
	targetRel string,
) error {
	if err := createProjectionParentDirs(root, filepath.Dir(targetRel)); err != nil {
		var moveErr *childProjectionMoveError
		if errors.As(err, &moveErr) {
			return err
		}
		if errors.Is(err, synctree.ErrUnsafePath) {
			return projectionConflict(move, "projection path ancestor is not a directory")
		}
		return projectionUnavailable(move, "creating current local path parent", err)
	}
	if err := root.Rename(sourceRel, targetRel); err != nil {
		return projectionUnavailable(move, "moving local projection", err)
	}

	return nil
}

func renameCaseOnlyProjection(move *childProjectionMove, root *synctree.Root, sourceRel string, targetRel string) error {
	if sourceRel == targetRel {
		return nil
	}

	tempStem := caseOnlyProjectionTempStem(move, targetRel)
	if err := root.RenameWithTemporarySibling(sourceRel, targetRel, tempStem, caseOnlyProjectionRenameAttempts); err != nil {
		return projectionUnavailable(move, "moving local projection from temporary path", err)
	}

	return nil
}

func caseOnlyProjectionTempStem(move *childProjectionMove, targetRel string) string {
	sum := sha256.Sum256([]byte(move.mountID.String() + "\x00" + targetRel))
	return ".onedrive-go-projection-rename-" + hex.EncodeToString(sum[:])[:16]
}

func childProjectionRelativePath(rel string) (string, error) {
	cleanRel := filepath.Clean(filepath.FromSlash(rel))
	if cleanRel == "." || cleanRel == "" || filepath.IsAbs(cleanRel) {
		return "", fmt.Errorf("path %q must be relative", rel)
	}
	if cleanRel == parentPathSegment ||
		strings.HasPrefix(cleanRel, parentPathSegment+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes parent sync root", rel)
	}

	return cleanRel, nil
}

func projectionPathState(root *synctree.Root, rel string) (synctree.PathState, error) {
	state, err := root.PathStateNoFollow(rel)
	if err != nil {
		return synctree.PathState{}, fmt.Errorf("reading projection path %s: %w", rel, err)
	}

	return state, nil
}

func validateProjectionAncestors(move *childProjectionMove, root *synctree.Root, rel string) error {
	err := root.ValidateNoSymlinkAncestors(rel)
	if errors.Is(err, synctree.ErrUnsafePath) {
		return projectionConflict(move, "projection path ancestor is not a directory")
	}
	if err != nil {
		return projectionUnavailable(move, "checking projection path ancestor", err)
	}

	return nil
}

func createProjectionParentDirs(root *synctree.Root, targetParentRel string) error {
	if err := root.MkdirAllNoFollow(targetParentRel, childMountRootDirPerms); err != nil {
		return fmt.Errorf("creating projection parent directories: %w", err)
	}

	return nil
}

func projectionPathsSameFile(root *synctree.Root, sourceRel string, targetRel string) (bool, error) {
	same, err := root.SameFile(sourceRel, targetRel)
	if err != nil {
		return false, fmt.Errorf("comparing projection paths: %w", err)
	}

	return same, nil
}

func recordProjectionMoveSuccess(move *childProjectionMove) error {
	parent := &mountSpec{syncRoot: move.parentSyncRoot}
	recordForPath := config.MountRecord{RelativeLocalPath: move.toRelativeLocalPath}
	identity, identityErr := rootIdentityForRecordPath(parent, &recordForPath)
	if identityErr != nil {
		identity = nil
	}
	if err := config.UpdateMountInventory(func(inventory *config.MountInventory) error {
		record, found := inventory.Mounts[move.mountID.String()]
		if !found {
			return nil
		}

		plan, err := planProjectionMoveSuccess(&record, move, identity)
		if err != nil {
			return err
		}
		inventory.Mounts[record.MountID] = plan.Record
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

	return recordShortcutLifecyclePlan(
		move.mountID,
		logger,
		"recording child projection move failure",
		func(record *config.MountRecord) (shortcutLifecyclePlan, error) {
			return planProjectionMoveFailure(record, move, failure.state, failure.reason)
		},
	)
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
