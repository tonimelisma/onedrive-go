package multisync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

type childRootLifecycleActionError struct {
	state  config.MountState
	reason config.MountStateReason
	err    error
}

func (e *childRootLifecycleActionError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	if e.reason == "" {
		return e.err.Error()
	}

	return fmt.Sprintf("%s: %s", e.reason, e.err.Error())
}

func (e *childRootLifecycleActionError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.err
}

func applyChildRootLifecycleActions(
	ctx context.Context,
	orchestrator *Orchestrator,
	compiled *compiledMountSet,
	logger *slog.Logger,
) bool {
	if compiled == nil || len(compiled.LocalRootActions) == 0 {
		return false
	}

	changed := false
	failed := make(map[mountID]struct{})
	for i := range compiled.LocalRootActions {
		action := &compiled.LocalRootActions[i]
		if _, alreadyFailed := failed[action.mountID]; alreadyFailed {
			continue
		}

		if err := applyChildRootLifecycleAction(ctx, orchestrator, action); err != nil {
			if recordChildRootLifecycleActionFailure(action, err, logger) {
				changed = true
			}
			compiled.Skipped = append(compiled.Skipped, mountStartupResultForChildRootLifecycleAction(action, err))
			failed[action.mountID] = struct{}{}
			continue
		}

		if err := recordChildRootLifecycleActionSuccess(action); err != nil {
			wrapped := fmt.Errorf("recording child root lifecycle action for mount %s: %w", action.mountID, err)
			compiled.Skipped = append(compiled.Skipped, mountStartupResultForChildRootLifecycleAction(action, wrapped))
			failed[action.mountID] = struct{}{}
			continue
		}
		if action.kind == childRootLifecycleActionDelete {
			compiled.RemovedMountIDs = appendUniqueStrings(compiled.RemovedMountIDs, action.mountID.String())
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

	filteredMoves := compiled.ProjectionMoves[:0]
	for i := range compiled.ProjectionMoves {
		move := compiled.ProjectionMoves[i]
		if _, skip := failed[move.mountID]; skip {
			continue
		}
		filteredMoves = append(filteredMoves, move)
	}
	compiled.ProjectionMoves = filteredMoves
	return changed
}

func applyChildRootLifecycleAction(
	ctx context.Context,
	orchestrator *Orchestrator,
	action *childRootLifecycleAction,
) error {
	if action == nil {
		return nil
	}
	if orchestrator == nil || orchestrator.cfg == nil || orchestrator.cfg.Runtime == nil {
		return childRootLifecycleActionUnavailable(action, "opening parent session", fmt.Errorf("session runtime is unavailable"))
	}
	if action.parent == nil {
		return childRootLifecycleActionUnavailable(action, "opening parent session", fmt.Errorf("parent mount is missing"))
	}

	session, err := orchestrator.cfg.Runtime.SyncSession(ctx, action.parent.syncSessionConfig())
	if err != nil {
		return childRootLifecycleActionUnavailable(action, "opening parent session", err)
	}

	switch action.kind {
	case childRootLifecycleActionRename:
		alias := childRootActionAlias(action)
		if alias == "" {
			return childRootLifecycleActionConflict(action, "renamed local alias is empty")
		}
		if _, err := session.MoveItem(ctx, action.bindingItemID, "", alias); err != nil {
			return childRootLifecycleActionUnavailable(action, "renaming shortcut placeholder", err)
		}
		return nil
	case childRootLifecycleActionDelete:
		if err := session.DeleteItem(ctx, action.bindingItemID); err != nil && !driveops.IsNotFound(err) {
			return childRootLifecycleActionUnavailable(action, "deleting shortcut placeholder", err)
		}
		return nil
	default:
		return childRootLifecycleActionUnavailable(
			action,
			"classifying child root lifecycle action",
			fmt.Errorf("unknown action %q", action.kind),
		)
	}
}

func recordChildRootLifecycleActionSuccess(action *childRootLifecycleAction) error {
	var identity *config.RootIdentity
	if action.kind == childRootLifecycleActionRename {
		record := config.MountRecord{RelativeLocalPath: action.toRelativeLocalPath}
		captured, err := rootIdentityForRecordPath(action.parent, &record)
		if err == nil {
			identity = captured
		}
	}
	if err := config.UpdateMountInventory(func(inventory *config.MountInventory) error {
		record, found := inventory.Mounts[action.mountID.String()]
		if !found {
			return nil
		}

		plan, err := planChildRootLifecycleActionSuccess(&record, action, identity)
		if err != nil {
			return err
		}
		inventory.Mounts[record.MountID] = plan.Record
		return nil
	}); err != nil {
		return fmt.Errorf("updating mount inventory after child root lifecycle action: %w", err)
	}

	return nil
}

func recordChildRootLifecycleActionFailure(
	action *childRootLifecycleAction,
	err error,
	logger *slog.Logger,
) bool {
	var failure *childRootLifecycleActionError
	if !errors.As(err, &failure) {
		failure = &childRootLifecycleActionError{
			state:  config.MountStateUnavailable,
			reason: fallbackChildRootLifecycleFailureReason(action),
			err:    err,
		}
	}

	return recordShortcutLifecyclePlan(
		action.mountID,
		logger,
		"recording child root lifecycle action failure",
		func(record *config.MountRecord) (shortcutLifecyclePlan, error) {
			return planChildRootLifecycleActionFailure(record, action, failure.state, failure.reason)
		},
	)
}

func fallbackChildRootLifecycleFailureReason(action *childRootLifecycleAction) config.MountStateReason {
	if action != nil && action.kind == childRootLifecycleActionDelete {
		return config.MountStateReasonLocalAliasDeleteUnavailable
	}

	return config.MountStateReasonLocalAliasRenameUnavailable
}

func childRootLifecycleActionConflict(action *childRootLifecycleAction, message string) error {
	return &childRootLifecycleActionError{
		state:  config.MountStateConflict,
		reason: config.MountStateReasonLocalAliasRenameConflict,
		err: fmt.Errorf(
			"child mount %s is conflicted: %s: %s",
			action.mountID,
			config.MountStateReasonLocalAliasRenameConflict,
			message,
		),
	}
}

func childRootLifecycleActionUnavailable(
	action *childRootLifecycleAction,
	operation string,
	err error,
) error {
	return &childRootLifecycleActionError{
		state:  config.MountStateUnavailable,
		reason: fallbackChildRootLifecycleFailureReason(action),
		err:    fmt.Errorf("%s for child mount %s: %w", operation, action.mountID, err),
	}
}

func mountStartupResultForChildRootLifecycleAction(action *childRootLifecycleAction, err error) MountStartupResult {
	return MountStartupResult{
		SelectionIndex: action.selectionIndex,
		Identity:       action.identity,
		DisplayName:    action.displayName,
		Status:         classifyMountStartupError(err),
		Err:            err,
	}
}

func rootIdentityForRecordPath(parent *mountSpec, record *config.MountRecord) (*config.RootIdentity, error) {
	if parent == nil {
		return nil, fmt.Errorf("parent mount is missing")
	}
	relativePath, ok := cleanChildMountRootRelativePath(record.RelativeLocalPath)
	if !ok {
		return nil, fmt.Errorf("path %q escapes parent sync root", record.RelativeLocalPath)
	}
	root, err := synctree.Open(parent.syncRoot)
	if err != nil {
		return nil, fmt.Errorf("opening parent sync root: %w", err)
	}
	if validateErr := root.ValidateNoSymlinkAncestors(relativePath); validateErr != nil {
		return nil, fmt.Errorf("validating child root ancestors: %w", validateErr)
	}
	identity, err := childRootIdentity(root, relativePath)
	if err != nil {
		return nil, fmt.Errorf("capturing child root identity: %w", err)
	}

	return identity, nil
}
