package sync

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	stdsync "sync"

	"golang.org/x/sync/errgroup"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// maxShortcutConcurrency limits how many shortcut scopes are observed in parallel.
const maxShortcutConcurrency = 4

// filterOutShortcuts removes synctypes.ChangeShortcut events from a slice — they are
// consumed by processShortcuts and should not enter the planner as regular events.
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

// processShortcuts extracts shortcut events from the primary delta, updates
// the shortcuts table, and observes content on each shortcut's source drive.
// Returns additional ChangeEvents for shortcut content that should be fed
// into the planner alongside primary drive events.
func (coordinator *shortcutCoordinator) processShortcuts(
	ctx context.Context,
	remoteEvents []synctypes.ChangeEvent,
	bl *synctypes.Baseline,
	dryRun bool,
	suppressedShortcutTargets map[string]struct{},
) ([]synctypes.ChangeEvent, error) {
	flow := coordinator.flow
	eng := flow.engine

	if eng.folderDelta == nil && eng.recursiveLister == nil {
		return nil, nil
	}

	// Step 1: Extract shortcut events and detect removed shortcuts.
	var shortcutEvents []synctypes.ChangeEvent

	removedShortcutIDs := make(map[string]bool)

	for i := range remoteEvents {
		if remoteEvents[i].Type == synctypes.ChangeShortcut {
			shortcutEvents = append(shortcutEvents, remoteEvents[i])
			continue
		}

		if remoteEvents[i].Type == synctypes.ChangeDelete {
			removedShortcutIDs[remoteEvents[i].ItemID] = true
		}
	}

	// Step 2: Handle removed shortcuts.
	// B-334: Pre-filter delete IDs to known shortcuts before processing.
	if len(removedShortcutIDs) > 0 {
		preFilterShortcuts, err := eng.baseline.ListShortcuts(ctx)
		if err != nil {
			return nil, fmt.Errorf("sync: listing shortcuts for removal pre-filter: %w", err)
		}

		known := make(map[string]bool, len(preFilterShortcuts))
		for i := range preFilterShortcuts {
			known[preFilterShortcuts[i].ItemID] = true
		}

		for id := range removedShortcutIDs {
			if !known[id] {
				delete(removedShortcutIDs, id)
			}
		}

		if err := coordinator.handleRemovedShortcuts(ctx, removedShortcutIDs, preFilterShortcuts); err != nil {
			return nil, err
		}
	}

	// Step 3: Register/update shortcuts from shortcut events.
	if err := coordinator.registerShortcuts(ctx, shortcutEvents); err != nil {
		return nil, err
	}

	// B-333: Load shortcuts once after registration and thread through.
	shortcuts, err := eng.baseline.ListShortcuts(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: listing shortcuts: %w", err)
	}
	flow.setShortcuts(shortcuts)

	// Step 4: Detect path collisions and skip colliding shortcuts.
	collisions := detectShortcutCollisionsFromList(shortcuts, bl, eng.logger)

	if dryRun {
		return nil, nil
	}

	// Step 5: Observe content for all active shortcuts (excluding collisions and
	// any shared targets currently rate limited).
	return coordinator.observeShortcutContentFromList(ctx, shortcuts, bl, collisions, suppressedShortcutTargets)
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

// scopeResult holds the observation output for a single shortcut scope.
// Delta tokens are collected and committed only after all scopes succeed,
// preventing partial token advancement on crash.
type scopeResult struct {
	events     []synctypes.ChangeEvent
	deltaToken string // non-empty for delta-observed scopes
	shortcut   *synctypes.Shortcut
}

// shortcutDispatchFunc is the per-scope callback used by
// observeShortcutTargetsConcurrently. Each caller supplies a closure that
// performs the actual observation for one planned shortcut target.
type shortcutDispatchFunc func(ctx context.Context, target plannedObservationTarget) (scopeResult, error)

// observeShortcutTargetsConcurrently fans out shortcut observation across up to
// maxShortcutConcurrency goroutines. It handles errgroup setup, collision
// skipping, error logging, result collection, delta token commit, and
// completion logging. The dispatch func contains the per-scope observation
// strategy — callers only need to supply a closure and a log label.
func (coordinator *shortcutCoordinator) observeShortcutTargetsConcurrently(
	ctx context.Context,
	phase ObservationPhasePlan,
	dispatch shortcutDispatchFunc, logContext string,
) ([]synctypes.ChangeEvent, error) {
	flow := coordinator.flow
	eng := flow.engine

	if !phase.HasTargets() {
		return nil, nil
	}
	if phase.FallbackPolicy != observationPhaseFallbackPolicyNone {
		return nil, fmt.Errorf("sync: shortcut %s: unsupported fallback policy %q", logContext, phase.FallbackPolicy)
	}

	results := make([]scopeResult, len(phase.Targets))

	var mu stdsync.Mutex // guards writes to results while shortcut workers finish out of order

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(maxShortcutConcurrency)

	for i := range phase.Targets {
		target := phase.Targets[i]

		g.Go(func() error {
			// Always set shortcut pointer so post-loop code can safely
			// dereference it even for failed/skipped scopes.
			mu.Lock()
			results[i].shortcut = target.shortcut
			mu.Unlock()

			result, scErr := dispatch(gCtx, target)
			if scErr != nil {
				if phase.ErrorPolicy == observationPhaseErrorPolicyIsolateTarget {
					eng.logger.Warn("shortcut "+logContext+" failed, skipping",
						slog.String("item_id", target.shortcut.ItemID),
						slog.String("remote_drive", target.shortcut.RemoteDrive),
						slog.String("error", scErr.Error()),
					)

					return nil
				}

				return fmt.Errorf("observe shortcut target %s: %w", target.shortcut.ItemID, scErr)
			}

			mu.Lock()
			results[i].events = result.events
			results[i].deltaToken = result.deltaToken
			mu.Unlock()

			return nil
		})
	}

	// errgroup goroutines never return errors (they log and continue),
	// but we check the error to satisfy errcheck lint.
	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("sync: shortcut %s: %w", logContext, err)
	}

	// Commit all delta tokens after all scopes are done.
	var allEvents []synctypes.ChangeEvent

	for i := range results {
		allEvents = append(allEvents, results[i].events...)

		if results[i].deltaToken != "" && phase.TokenCommitPolicy == observationPhaseTokenCommitPolicyAfterPhaseSuccess {
			if err := eng.baseline.CommitDeltaToken(
				ctx,
				results[i].deltaToken,
				results[i].shortcut.RemoteDrive,
				results[i].shortcut.RemoteItem,
				results[i].shortcut.RemoteDrive,
			); err != nil {
				eng.logger.Warn("failed to commit shortcut delta token",
					slog.String("item_id", results[i].shortcut.ItemID),
					slog.String("error", err.Error()),
				)
			}
		}
	}
	if phase.TokenCommitPolicy != observationPhaseTokenCommitPolicyAfterPhaseSuccess {
		for i := range results {
			if results[i].deltaToken != "" {
				return nil, fmt.Errorf("sync: shortcut %s: unsupported token commit policy %q", logContext, phase.TokenCommitPolicy)
			}
		}
	}

	if len(allEvents) > 0 {
		eng.logger.Info("shortcut "+logContext+" complete",
			slog.Int("shortcuts", len(phase.Targets)),
			slog.Int("events", len(allEvents)),
		)
	}

	return allEvents, nil
}

// observeShortcutContentFromList observes content for all active shortcuts
// concurrently. Takes pre-loaded shortcuts to avoid redundant DB queries
// (B-333). Delta tokens are deferred until all scopes complete, ensuring
// atomicity.
func (coordinator *shortcutCoordinator) observeShortcutContentFromList(
	ctx context.Context,
	shortcuts []synctypes.Shortcut,
	bl *synctypes.Baseline,
	collisions map[string]bool,
	suppressedShortcutTargets map[string]struct{},
) ([]synctypes.ChangeEvent, error) {
	if len(shortcuts) == 0 {
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

	return coordinator.observeShortcutTargetsConcurrently(ctx, plan.ShortcutPhase,
		func(gCtx context.Context, target plannedObservationTarget) (scopeResult, error) {
			return coordinator.observeShortcutTarget(gCtx, target, bl, false)
		},
		"observation",
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

	plan, err := flow.BuildObservationSessionPlan(ctx, ObservationPlanRequest{
		Baseline:  bl,
		SyncMode:  synctypes.SyncBidirectional,
		Purpose:   observationPlanPurposeOneShot,
		Shortcuts: shortcuts,
	})
	if err != nil {
		return nil, fmt.Errorf("sync: build shortcut reconciliation plan: %w", err)
	}

	return coordinator.observeShortcutTargetsConcurrently(ctx, plan.ShortcutPhase,
		func(gCtx context.Context, target plannedObservationTarget) (scopeResult, error) {
			return coordinator.observeShortcutTarget(gCtx, target, bl, true)
		},
		"reconciliation",
	)
}

func (coordinator *shortcutCoordinator) observeShortcutTarget(
	ctx context.Context,
	target plannedObservationTarget,
	bl *synctypes.Baseline,
	fullReconcile bool,
) (scopeResult, error) {
	result, err := coordinator.flow.observePlannedTarget(ctx, bl, target, fullReconcile)
	if err != nil {
		return scopeResult{}, err
	}

	outcome := scopeResult{
		events: result.events,
	}
	if target.shortcut != nil {
		outcome.shortcut = target.shortcut
	}
	if len(result.deferred) > 0 {
		outcome.deltaToken = result.deferred[0].token
	}

	return outcome, nil
}
