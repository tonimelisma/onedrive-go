package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	stdsync "sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
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
			DiscoveredAt: time.Now().Unix(),
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
// observeShortcutsConcurrently. Each caller supplies a closure that performs
// the actual observation (delta, enumerate, or reconciliation).
type shortcutDispatchFunc func(ctx context.Context, sc *synctypes.Shortcut) (scopeResult, error)

// observeShortcutsConcurrently fans out shortcut observation across up to
// maxShortcutConcurrency goroutines. It handles errgroup setup, collision
// skipping, error logging, result collection, delta token commit, and
// completion logging. The dispatch func contains the per-scope observation
// strategy — callers only need to supply a closure and a log label.
func (coordinator *shortcutCoordinator) observeShortcutsConcurrently(
	ctx context.Context, shortcuts []synctypes.Shortcut, collisions map[string]bool,
	dispatch shortcutDispatchFunc, logContext string,
) ([]synctypes.ChangeEvent, error) {
	flow := coordinator.flow
	eng := flow.engine

	if len(shortcuts) == 0 {
		return nil, nil
	}

	results := make([]scopeResult, len(shortcuts))

	var mu stdsync.Mutex // guards writes to results while shortcut workers finish out of order

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(maxShortcutConcurrency)

	for i := range shortcuts {
		sc := &shortcuts[i]

		g.Go(func() error {
			// Always set shortcut pointer so post-loop code can safely
			// dereference it even for failed/skipped scopes.
			mu.Lock()
			results[i].shortcut = sc
			mu.Unlock()

			if collisions[sc.ItemID] {
				return nil
			}

			result, scErr := dispatch(gCtx, sc)
			if scErr != nil {
				eng.logger.Warn("shortcut "+logContext+" failed, skipping",
					slog.String("item_id", sc.ItemID),
					slog.String("remote_drive", sc.RemoteDrive),
					slog.String("error", scErr.Error()),
				)

				return nil // don't fail the group
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

		if results[i].deltaToken != "" {
			sc := results[i].shortcut
			if err := eng.baseline.CommitDeltaToken(ctx, results[i].deltaToken, sc.RemoteDrive, sc.RemoteItem, sc.RemoteDrive); err != nil {
				eng.logger.Warn("failed to commit shortcut delta token",
					slog.String("item_id", sc.ItemID),
					slog.String("error", err.Error()),
				)
			}
		}
	}

	if len(allEvents) > 0 {
		eng.logger.Info("shortcut "+logContext+" complete",
			slog.Int("shortcuts", len(shortcuts)),
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
	filtered := shortcuts
	if len(suppressedShortcutTargets) > 0 {
		filtered = make([]synctypes.Shortcut, 0, len(shortcuts))
		for i := range shortcuts {
			shortcutKey := shortcuts[i].RemoteDrive + ":" + shortcuts[i].RemoteItem
			if _, suppressed := suppressedShortcutTargets[shortcutKey]; suppressed {
				coordinator.flow.engine.logger.Debug(
					"suppressing shortcut observation — target rate limited",
					slog.String("shortcut_key", shortcutKey),
					slog.String("local_path", shortcuts[i].LocalPath),
				)
				continue
			}
			filtered = append(filtered, shortcuts[i])
		}
	}

	return coordinator.observeShortcutsConcurrently(ctx, filtered, collisions,
		func(gCtx context.Context, sc *synctypes.Shortcut) (scopeResult, error) {
			return coordinator.observeSingleShortcut(gCtx, sc, bl)
		},
		"observation",
	)
}

// observeSingleShortcut observes content for one shortcut scope.
// Returns a scopeResult with events and an optional delta token to commit.
func (coordinator *shortcutCoordinator) observeSingleShortcut(
	ctx context.Context,
	sc *synctypes.Shortcut,
	bl *synctypes.Baseline,
) (scopeResult, error) {
	remoteDriveID := driveid.New(sc.RemoteDrive)

	switch sc.Observation {
	case synctypes.ObservationDelta:
		return coordinator.observeShortcutDelta(ctx, sc, remoteDriveID, bl)
	default:
		// B-335: synctypes.ObservationEnumerate and synctypes.ObservationUnknown both use enumerate.
		return coordinator.observeShortcutEnumerate(ctx, sc, remoteDriveID, bl)
	}
}

// observeShortcutDelta uses folder-scoped delta to observe shortcut content.
// Returns the events and the new delta token (not committed — caller handles that).
//
// Orphan detection is deliberately omitted here: incremental delta only returns
// items that changed since the last token, not the full set. Comparing that
// partial set against the baseline would incorrectly flag unchanged items as
// deleted. Orphan detection for delta-observed shortcuts is handled by
// reconcileShortcutDelta, which uses an empty token to enumerate all items.
func (coordinator *shortcutCoordinator) observeShortcutDelta(
	ctx context.Context, sc *synctypes.Shortcut, remoteDriveID driveid.ID, bl *synctypes.Baseline,
) (scopeResult, error) {
	flow := coordinator.flow
	eng := flow.engine

	if eng.folderDelta == nil {
		return scopeResult{}, fmt.Errorf("sync: folder delta not available for shortcut %s", sc.ItemID)
	}

	savedToken, err := eng.baseline.GetDeltaToken(ctx, sc.RemoteDrive, sc.RemoteItem)
	if err != nil {
		return scopeResult{}, fmt.Errorf("sync: getting shortcut delta token: %w", err)
	}

	items, newToken, err := eng.folderDelta.DeltaFolderAll(ctx, remoteDriveID, sc.RemoteItem, savedToken)
	if err != nil {
		if errors.Is(err, graph.ErrGone) {
			eng.logger.Warn("shortcut delta token expired, performing full resync",
				slog.String("item_id", sc.ItemID),
			)

			items, newToken, err = eng.folderDelta.DeltaFolderAll(ctx, remoteDriveID, sc.RemoteItem, "")
			if err != nil {
				return scopeResult{}, fmt.Errorf("sync: shortcut full resync: %w", err)
			}
		} else {
			return scopeResult{}, fmt.Errorf("sync: shortcut delta: %w", err)
		}
	}

	events := syncobserve.ConvertShortcutItems(items, sc, remoteDriveID, bl, eng.logger)

	return scopeResult{
		events:     events,
		deltaToken: newToken,
		shortcut:   sc,
	}, nil
}

// observeShortcutEnumerate uses recursive listing to observe shortcut content.
func (coordinator *shortcutCoordinator) observeShortcutEnumerate(
	ctx context.Context, sc *synctypes.Shortcut, remoteDriveID driveid.ID, bl *synctypes.Baseline,
) (scopeResult, error) {
	flow := coordinator.flow
	eng := flow.engine

	if eng.recursiveLister == nil {
		return scopeResult{}, fmt.Errorf("sync: recursive lister not available for shortcut %s", sc.ItemID)
	}

	items, err := eng.recursiveLister.ListChildrenRecursive(ctx, remoteDriveID, sc.RemoteItem)
	if err != nil {
		return scopeResult{}, fmt.Errorf("sync: shortcut enumerate: %w", err)
	}

	events := syncobserve.ConvertShortcutItems(items, sc, remoteDriveID, bl, eng.logger)

	// Detect deletions: items in baseline under this scope but not in enumeration.
	orphans := syncobserve.DetectShortcutOrphans(sc, remoteDriveID, items, bl)
	events = append(events, orphans...)

	return scopeResult{
		events:   events,
		shortcut: sc,
	}, nil
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

	collisions := detectShortcutCollisionsFromList(shortcuts, bl, eng.logger)

	return coordinator.observeShortcutsConcurrently(ctx, shortcuts, collisions,
		func(gCtx context.Context, sc *synctypes.Shortcut) (scopeResult, error) {
			remoteDriveID := driveid.New(sc.RemoteDrive)

			switch sc.Observation {
			case synctypes.ObservationDelta:
				return coordinator.reconcileShortcutDelta(gCtx, sc, remoteDriveID, bl)
			default:
				return coordinator.observeShortcutEnumerate(gCtx, sc, remoteDriveID, bl)
			}
		},
		"reconciliation",
	)
}

// reconcileShortcutDelta performs a full delta enumeration for a shortcut
// by using an empty token. This enumerates all items via delta and detects
// orphans that may have been missed by incremental delta.
func (coordinator *shortcutCoordinator) reconcileShortcutDelta(
	ctx context.Context, sc *synctypes.Shortcut, remoteDriveID driveid.ID, bl *synctypes.Baseline,
) (scopeResult, error) {
	flow := coordinator.flow
	eng := flow.engine

	if eng.folderDelta == nil {
		return scopeResult{}, fmt.Errorf("sync: folder delta not available for shortcut %s", sc.ItemID)
	}

	items, newToken, err := eng.folderDelta.DeltaFolderAll(ctx, remoteDriveID, sc.RemoteItem, "")
	if err != nil {
		return scopeResult{}, fmt.Errorf("sync: shortcut full reconciliation delta: %w", err)
	}

	events := syncobserve.ConvertShortcutItems(items, sc, remoteDriveID, bl, eng.logger)
	orphans := syncobserve.DetectShortcutOrphans(sc, remoteDriveID, items, bl)
	events = append(events, orphans...)

	return scopeResult{
		events:     events,
		deltaToken: newToken,
		shortcut:   sc,
	}, nil
}
