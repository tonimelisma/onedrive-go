package sync

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// maxShortcutConcurrency limits how many shortcut scopes are observed in parallel.
const maxShortcutConcurrency = 4

type shortcutBatchMutationResult struct {
	VisiblePrimary []synctypes.ChangeEvent
	Shortcuts      []synctypes.Shortcut
}

// filterOutShortcuts removes synctypes.ChangeShortcut events from a slice —
// shortcut discovery is consumed by the post-primary coordinator and should
// not enter the planner as regular path events.
func filterOutShortcuts(events []synctypes.ChangeEvent) []synctypes.ChangeEvent {
	n := 0

	for i := range events {
		if events[i].Type != synctypes.ChangeShortcut {
			events[n] = events[i]
			n++
		}
	}

	return events[:n]
}

// registerShortcuts upserts shortcuts from synctypes.ChangeShortcut events.
// For new shortcuts, detects the source drive type to determine the
// observation strategy (delta vs enumerate).
func (coordinator *shortcutCoordinator) registerShortcuts(ctx context.Context, events []synctypes.ChangeEvent) error {
	flow := coordinator.flow
	eng := flow.engine

	for i := range events {
		ev := &events[i]

		existing, found, err := eng.baseline.GetShortcut(ctx, ev.ItemID)
		if err != nil {
			return fmt.Errorf("sync: checking shortcut %s: %w", ev.ItemID, err)
		}

		sc := synctypes.Shortcut{
			ItemID:       ev.ItemID,
			RemoteDrive:  ev.RemoteDriveID,
			RemoteItem:   ev.RemoteItemID,
			LocalPath:    ev.Path,
			Observation:  synctypes.ObservationUnknown,
			DiscoveredAt: eng.nowFunc().Unix(),
		}

		// Preserve existing values on update.
		if found {
			sc.DriveType = existing.DriveType
			sc.Observation = existing.Observation
			sc.DiscoveredAt = existing.DiscoveredAt
		}

		// Detect drive type for new shortcuts.
		if sc.DriveType == "" && eng.driveVerifier != nil {
			driveType, obsStrategy := coordinator.detectDriveType(ctx, ev.RemoteDriveID)
			sc.DriveType = driveType
			sc.Observation = obsStrategy
		}

		if err := eng.baseline.UpsertShortcut(ctx, &sc); err != nil {
			return fmt.Errorf("sync: registering shortcut %s: %w", ev.ItemID, err)
		}

		eng.logger.Info("registered shortcut",
			slog.String("item_id", sc.ItemID),
			slog.String("local_path", sc.LocalPath),
			slog.String("remote_drive", sc.RemoteDrive),
			slog.String("remote_item", sc.RemoteItem),
			slog.String("observation", sc.Observation),
		)
	}

	return nil
}

// detectShortcutCollisionsFromList checks for path conflicts between shortcuts
// and returns the set of item IDs that should be skipped during observation.
// Detects: exact duplicates, prefix nesting (Shared vs Shared/Sub), and
// conflicts with primary drive baseline entries.
func detectShortcutCollisionsFromList(shortcuts []synctypes.Shortcut, bl *synctypes.Baseline, logger *slog.Logger) map[string]bool {
	collisions := make(map[string]bool)

	// Check shortcut-vs-shortcut: exact duplicates and prefix nesting.
	paths := make(map[string]string, len(shortcuts)) // localPath → itemID

	for i := range shortcuts {
		sc := &shortcuts[i]

		// Exact duplicate check.
		if existing, ok := paths[sc.LocalPath]; ok {
			logger.Warn("shortcut path collision: duplicate path, skipping later shortcut",
				slog.String("path", sc.LocalPath),
				slog.String("kept", existing),
				slog.String("skipped", sc.ItemID),
			)

			collisions[sc.ItemID] = true

			continue
		}

		// Prefix nesting check: is this path a parent or child of an existing shortcut?
		for existingPath, existingID := range paths {
			if strings.HasPrefix(sc.LocalPath, existingPath+"/") {
				logger.Warn("shortcut path collision: nested under existing shortcut, skipping",
					slog.String("parent_path", existingPath),
					slog.String("child_path", sc.LocalPath),
					slog.String("skipped", sc.ItemID),
				)

				collisions[sc.ItemID] = true

				break
			}

			if strings.HasPrefix(existingPath, sc.LocalPath+"/") {
				logger.Warn("shortcut path collision: existing shortcut nested under new, skipping existing",
					slog.String("parent_path", sc.LocalPath),
					slog.String("child_path", existingPath),
					slog.String("skipped", existingID),
				)

				collisions[existingID] = true
			}
		}

		paths[sc.LocalPath] = sc.ItemID
	}

	// Check shortcut-vs-shortcut: duplicate source folder.
	// Sort by ItemID for deterministic winner selection (lowest ID wins).
	sorted := make([]synctypes.Shortcut, len(shortcuts))
	copy(sorted, shortcuts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ItemID < sorted[j].ItemID })

	sources := make(map[string]string, len(sorted)) // "remoteDrive:remoteItem" → itemID

	for i := range sorted {
		sc := &sorted[i]
		if collisions[sc.ItemID] {
			continue
		}

		key := sc.RemoteDrive + ":" + sc.RemoteItem
		if existing, ok := sources[key]; ok {
			logger.Warn("shortcut source collision: duplicate source folder, skipping",
				slog.String("remote_drive", sc.RemoteDrive),
				slog.String("remote_item", sc.RemoteItem),
				slog.String("kept", existing),
				slog.String("skipped", sc.ItemID),
			)

			collisions[sc.ItemID] = true

			continue
		}

		sources[key] = sc.ItemID
	}

	// Check shortcut-vs-primary-drive collisions.
	for i := range shortcuts {
		sc := &shortcuts[i]
		if collisions[sc.ItemID] {
			continue
		}

		remoteDriveID := driveid.New(sc.RemoteDrive)

		if entry, ok := bl.GetByPath(sc.LocalPath); ok && entry.DriveID != remoteDriveID {
			logger.Warn("shortcut path collision: conflicts with primary drive, skipping",
				slog.String("path", sc.LocalPath),
				slog.String("shortcut", sc.ItemID),
				slog.String("primary_item", entry.ItemID),
			)

			collisions[sc.ItemID] = true
		}
	}

	return collisions
}

// detectDriveType queries the source drive to determine its type and the
// appropriate observation strategy.
func (coordinator *shortcutCoordinator) detectDriveType(ctx context.Context, remoteDriveID string) (string, string) {
	flow := coordinator.flow
	eng := flow.engine

	drive, err := eng.driveVerifier.Drive(ctx, driveid.New(remoteDriveID))
	if err != nil {
		eng.logger.Warn("failed to detect source drive type, defaulting to enumerate",
			slog.String("remote_drive", remoteDriveID),
			slog.String("error", err.Error()),
		)

		return "", synctypes.ObservationEnumerate
	}

	driveType := drive.DriveType
	obs := synctypes.ObservationEnumerate

	if driveType == "personal" {
		obs = synctypes.ObservationDelta
	}

	return driveType, obs
}

// handleRemovedShortcuts processes synctypes.ChangeDelete events for known shortcuts.
// Takes pre-loaded shortcuts to avoid a redundant DB query.
func (coordinator *shortcutCoordinator) handleRemovedShortcuts(
	ctx context.Context,
	deletedItemIDs map[string]bool,
	shortcuts []synctypes.Shortcut,
) error {
	flow := coordinator.flow
	eng := flow.engine

	if len(deletedItemIDs) == 0 {
		return nil
	}

	for i := range shortcuts {
		if !deletedItemIDs[shortcuts[i].ItemID] {
			continue
		}

		sc := &shortcuts[i]

		eng.logger.Info("removing shortcut",
			slog.String("item_id", sc.ItemID),
			slog.String("local_path", sc.LocalPath),
			slog.String("remote_drive", sc.RemoteDrive),
		)

		// Delete delta token for this scope.
		if err := eng.baseline.DeleteDeltaToken(ctx, sc.RemoteDrive, sc.RemoteItem); err != nil {
			eng.logger.Warn("failed to delete shortcut delta token",
				slog.String("item_id", sc.ItemID),
				slog.String("error", err.Error()),
			)
		}

		if err := eng.baseline.DeleteShortcut(ctx, sc.ItemID); err != nil {
			return fmt.Errorf("sync: deleting shortcut %s: %w", sc.ItemID, err)
		}

		flow.scopeController().applyShortcutRemovalDecisionsWithWatch(ctx, flow.watch, coordinator.shortcutRemovalDecisions(ctx, sc))
		if flow.watch != nil {
			if err := flow.scopeController().loadActiveScopes(ctx, flow.watch); err != nil {
				return fmt.Errorf("sync: rebuilding active scopes after shortcut removal %s: %w", sc.ItemID, err)
			}
		}
	}

	return nil
}

// shortcutRemovalDecisions returns the scopes that become invalid when a
// shortcut disappears. These scopes are discarded, not released, because the
// blocked subtree itself no longer exists.
func (coordinator *shortcutCoordinator) shortcutRemovalDecisions(ctx context.Context, sc *synctypes.Shortcut) []ShortcutRemovalDecision {
	flow := coordinator.flow
	eng := flow.engine

	decisions := []ShortcutRemovalDecision{{
		ScopeKey: synctypes.SKQuotaShortcut(sc.RemoteDrive + ":" + sc.RemoteItem),
	}}

	issues, err := eng.baseline.ListRemoteBlockedFailures(ctx)
	if err != nil {
		eng.logger.Warn("failed to list remote permission scopes for removed shortcut",
			slog.String("shortcut_path", sc.LocalPath),
			slog.String("error", err.Error()),
		)
		return decisions
	}

	seen := map[synctypes.ScopeKey]bool{
		decisions[0].ScopeKey: true,
	}

	for i := range issues {
		issue := &issues[i]
		if !issue.ScopeKey.IsPermRemote() {
			continue
		}

		boundary := issue.ScopeKey.RemotePath()
		if boundary != sc.LocalPath && !strings.HasPrefix(boundary, sc.LocalPath+"/") {
			continue
		}

		if seen[issue.ScopeKey] {
			continue
		}

		seen[issue.ScopeKey] = true
		decisions = append(decisions, ShortcutRemovalDecision{ScopeKey: issue.ScopeKey})
	}

	return decisions
}

func (coordinator *shortcutCoordinator) applyShortcutBatchMutations(
	ctx context.Context,
	primaryEvents []synctypes.ChangeEvent,
	loadCurrentSnapshot bool,
) (shortcutBatchMutationResult, error) {
	flow := coordinator.flow
	eng := flow.engine
	shortcutEvents, removedShortcutIDs := splitShortcutBatchEvents(primaryEvents)
	result := shortcutBatchMutationResult{
		VisiblePrimary: filterOutShortcuts(primaryEvents),
	}

	if len(shortcutEvents) == 0 && len(removedShortcutIDs) == 0 {
		if !loadCurrentSnapshot {
			return result, nil
		}

		shortcuts, err := eng.baseline.ListShortcuts(ctx)
		if err != nil {
			return shortcutBatchMutationResult{}, fmt.Errorf("sync: listing shortcuts: %w", err)
		}
		flow.setShortcuts(shortcuts)
		result.Shortcuts = shortcuts
		return result, nil
	}

	if eng.baseline == nil {
		return shortcutBatchMutationResult{}, fmt.Errorf("sync: shortcut store unavailable")
	}
	if err := coordinator.removeShortcutsFromBatch(ctx, removedShortcutIDs); err != nil {
		return shortcutBatchMutationResult{}, err
	}
	if err := coordinator.registerShortcuts(ctx, shortcutEvents); err != nil {
		return shortcutBatchMutationResult{}, err
	}

	shortcuts, err := eng.baseline.ListShortcuts(ctx)
	if err != nil {
		return shortcutBatchMutationResult{}, fmt.Errorf("sync: listing shortcuts: %w", err)
	}

	flow.setShortcuts(shortcuts)
	result.Shortcuts = shortcuts
	return result, nil
}

func (coordinator *shortcutCoordinator) observeShortcutFollowUp(
	ctx context.Context,
	shortcuts []synctypes.Shortcut,
	bl *synctypes.Baseline,
	fullReconcile bool,
	collisions map[string]bool,
	suppressedShortcutTargets map[string]struct{},
) ([]synctypes.ChangeEvent, error) {
	eng := coordinator.flow.engine

	if len(shortcuts) == 0 || (eng.folderDelta == nil && eng.recursiveLister == nil) {
		return nil, nil
	}

	plan, err := coordinator.flow.BuildObservationSessionPlan(ctx, ObservationPlanRequest{
		Baseline:                  bl,
		SyncMode:                  synctypes.SyncBidirectional,
		Purpose:                   observationPlanPurposeOneShot,
		Shortcuts:                 shortcuts,
		ShortcutCollisions:        collisions,
		SuppressedShortcutTargets: suppressedShortcutTargets,
	})
	if err != nil {
		return nil, fmt.Errorf("sync: build shortcut observation plan: %w", err)
	}

	result, err := coordinator.flow.executeObservationPhase(ctx, bl, plan.ShortcutPhase, fullReconcile)
	if err != nil {
		return nil, err
	}

	return result.events, nil
}

// observeShortcutContentFromList observes content for all active shortcuts.
// Takes pre-loaded shortcuts to avoid redundant DB queries (B-333).
func (coordinator *shortcutCoordinator) observeShortcutContentFromList(
	ctx context.Context,
	shortcuts []synctypes.Shortcut,
	bl *synctypes.Baseline,
	collisions map[string]bool,
) ([]synctypes.ChangeEvent, error) {
	return coordinator.observeShortcutFollowUp(
		ctx,
		shortcuts,
		bl,
		false,
		collisions,
		nil,
	)
}

// reconcileShortcutScopes performs full reconciliation for all active shortcut
// scopes. For delta-capable shortcuts, runs a fresh delta with empty token.
// For enumerate-capable shortcuts, runs ListChildrenRecursive. Both detect
// orphans against the baseline.
func (coordinator *shortcutCoordinator) reconcileShortcutScopes(
	ctx context.Context,
	bl *synctypes.Baseline,
) ([]synctypes.ChangeEvent, error) {
	flow := coordinator.flow
	eng := flow.engine

	if eng.folderDelta == nil && eng.recursiveLister == nil {
		return nil, nil
	}

	shortcuts, err := eng.baseline.ListShortcuts(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: listing shortcuts for reconciliation: %w", err)
	}

	if len(shortcuts) == 0 {
		return nil, nil
	}

	return coordinator.observeShortcutFollowUp(ctx, shortcuts, bl, true, nil, nil)
}

func splitShortcutBatchEvents(primaryEvents []synctypes.ChangeEvent) ([]synctypes.ChangeEvent, map[string]bool) {
	var shortcutEvents []synctypes.ChangeEvent
	removedShortcutIDs := make(map[string]bool)

	for i := range primaryEvents {
		switch primaryEvents[i].Type {
		case synctypes.ChangeShortcut:
			shortcutEvents = append(shortcutEvents, primaryEvents[i])
		case synctypes.ChangeDelete:
			removedShortcutIDs[primaryEvents[i].ItemID] = true
		case synctypes.ChangeCreate, synctypes.ChangeModify, synctypes.ChangeMove:
			// These do not affect shortcut registration/removal directly.
		}
	}

	return shortcutEvents, removedShortcutIDs
}

func (coordinator *shortcutCoordinator) removeShortcutsFromBatch(
	ctx context.Context,
	removedShortcutIDs map[string]bool,
) error {
	if len(removedShortcutIDs) == 0 {
		return nil
	}

	eng := coordinator.flow.engine
	preFilterShortcuts, err := eng.baseline.ListShortcuts(ctx)
	if err != nil {
		return fmt.Errorf("sync: listing shortcuts for removal pre-filter: %w", err)
	}

	knownShortcutIDs := make(map[string]bool, len(preFilterShortcuts))
	for i := range preFilterShortcuts {
		knownShortcutIDs[preFilterShortcuts[i].ItemID] = true
	}
	for id := range removedShortcutIDs {
		if !knownShortcutIDs[id] {
			delete(removedShortcutIDs, id)
		}
	}

	if err := coordinator.handleRemovedShortcuts(ctx, removedShortcutIDs, preFilterShortcuts); err != nil {
		return err
	}

	return nil
}
