package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	stdsync "sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// maxShortcutConcurrency limits how many shortcut scopes are observed in parallel.
const maxShortcutConcurrency = 4

// processShortcuts extracts shortcut events from the primary delta, updates
// the shortcuts table, and observes content on each shortcut's source drive.
// Returns additional ChangeEvents for shortcut content that should be fed
// into the planner alongside primary drive events.
func (e *Engine) processShortcuts(
	ctx context.Context, remoteEvents []ChangeEvent, bl *Baseline, dryRun bool,
) ([]ChangeEvent, error) {
	if e.folderDelta == nil && e.recursiveLister == nil {
		return nil, nil
	}

	// Step 1: Extract shortcut events and detect removed shortcuts.
	var shortcutEvents []ChangeEvent

	removedShortcutIDs := make(map[string]bool)

	for i := range remoteEvents {
		switch remoteEvents[i].Type { //nolint:exhaustive // only ChangeShortcut and ChangeDelete are relevant here
		case ChangeShortcut:
			shortcutEvents = append(shortcutEvents, remoteEvents[i])
		case ChangeDelete:
			removedShortcutIDs[remoteEvents[i].ItemID] = true
		}
	}

	// B-334: Pre-filter delete IDs to known shortcuts before loading the full list.
	// This avoids unnecessary work in handleRemovedShortcuts.
	if len(removedShortcutIDs) > 0 {
		shortcuts, err := e.baseline.ListShortcuts(ctx)
		if err != nil {
			return nil, fmt.Errorf("sync: listing shortcuts for removal pre-filter: %w", err)
		}

		known := make(map[string]bool, len(shortcuts))
		for i := range shortcuts {
			known[shortcuts[i].ItemID] = true
		}

		for id := range removedShortcutIDs {
			if !known[id] {
				delete(removedShortcutIDs, id)
			}
		}
	}

	// Step 2: Handle removed shortcuts.
	if err := e.handleRemovedShortcuts(ctx, removedShortcutIDs); err != nil {
		return nil, err
	}

	// Step 3: Register/update shortcuts from shortcut events.
	if err := e.registerShortcuts(ctx, shortcutEvents); err != nil {
		return nil, err
	}

	// B-333: Load shortcuts once after registration and thread through.
	shortcuts, err := e.baseline.ListShortcuts(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: listing shortcuts: %w", err)
	}

	// Step 4: Detect path collisions and skip colliding shortcuts.
	collisions := detectShortcutCollisionsFromList(shortcuts, bl, e.logger)

	if dryRun {
		return nil, nil
	}

	// Step 5: Observe content for all active shortcuts (excluding collisions).
	return e.observeShortcutContentFromList(ctx, shortcuts, bl, collisions)
}

// registerShortcuts upserts shortcuts from ChangeShortcut events.
// For new shortcuts, detects the source drive type to determine the
// observation strategy (delta vs enumerate).
func (e *Engine) registerShortcuts(ctx context.Context, events []ChangeEvent) error {
	for i := range events {
		ev := &events[i]

		existing, err := e.baseline.GetShortcut(ctx, ev.ItemID)
		if err != nil {
			return fmt.Errorf("sync: checking shortcut %s: %w", ev.ItemID, err)
		}

		sc := Shortcut{
			ItemID:       ev.ItemID,
			RemoteDrive:  ev.RemoteDriveID,
			RemoteItem:   ev.RemoteItemID,
			LocalPath:    ev.Path,
			Observation:  ObservationUnknown,
			DiscoveredAt: time.Now().Unix(),
		}

		// Preserve existing values on update.
		if existing != nil {
			sc.DriveType = existing.DriveType
			sc.Observation = existing.Observation
			sc.DiscoveredAt = existing.DiscoveredAt
		}

		// Detect drive type for new shortcuts.
		if sc.DriveType == "" && e.driveVerifier != nil {
			driveType, obsStrategy := e.detectDriveType(ctx, ev.RemoteDriveID)
			sc.DriveType = driveType
			sc.Observation = obsStrategy
		}

		if err := e.baseline.UpsertShortcut(ctx, &sc); err != nil {
			return fmt.Errorf("sync: registering shortcut %s: %w", ev.ItemID, err)
		}

		e.logger.Info("registered shortcut",
			slog.String("item_id", sc.ItemID),
			slog.String("local_path", sc.LocalPath),
			slog.String("remote_drive", sc.RemoteDrive),
			slog.String("remote_item", sc.RemoteItem),
			slog.String("observation", sc.Observation),
		)
	}

	return nil
}

// detectShortcutCollisions checks for path conflicts between shortcuts and
// returns the set of item IDs that should be skipped during observation.
// Detects: exact duplicates, prefix nesting (Shared vs Shared/Sub), and
// conflicts with primary drive baseline entries.
func (e *Engine) detectShortcutCollisions(ctx context.Context, bl *Baseline) map[string]bool {
	shortcuts, err := e.baseline.ListShortcuts(ctx)
	if err != nil {
		return nil
	}

	return detectShortcutCollisionsFromList(shortcuts, bl, e.logger)
}

// detectShortcutCollisionsFromList is the list-threaded variant of
// detectShortcutCollisions (B-333). Avoids a redundant ListShortcuts query
// when the caller already has the list.
func detectShortcutCollisionsFromList(shortcuts []Shortcut, bl *Baseline, logger *slog.Logger) map[string]bool {
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
	sources := make(map[string]string, len(shortcuts)) // "remoteDrive:remoteItem" → itemID

	for i := range shortcuts {
		sc := &shortcuts[i]
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
func (e *Engine) detectDriveType(ctx context.Context, remoteDriveID string) (string, string) {
	drive, err := e.driveVerifier.Drive(ctx, driveid.New(remoteDriveID))
	if err != nil {
		e.logger.Warn("failed to detect source drive type, defaulting to enumerate",
			slog.String("remote_drive", remoteDriveID),
			slog.String("error", err.Error()),
		)

		return "", ObservationEnumerate
	}

	driveType := drive.DriveType
	obs := ObservationEnumerate

	if driveType == "personal" {
		obs = ObservationDelta
	}

	return driveType, obs
}

// handleRemovedShortcuts processes ChangeDelete events for known shortcuts.
func (e *Engine) handleRemovedShortcuts(ctx context.Context, deletedItemIDs map[string]bool) error {
	if len(deletedItemIDs) == 0 {
		return nil
	}

	shortcuts, err := e.baseline.ListShortcuts(ctx)
	if err != nil {
		return fmt.Errorf("sync: listing shortcuts for removal: %w", err)
	}

	for i := range shortcuts {
		if !deletedItemIDs[shortcuts[i].ItemID] {
			continue
		}

		sc := &shortcuts[i]

		e.logger.Info("removing shortcut",
			slog.String("item_id", sc.ItemID),
			slog.String("local_path", sc.LocalPath),
			slog.String("remote_drive", sc.RemoteDrive),
		)

		// Delete delta token for this scope.
		if err := e.baseline.DeleteDeltaToken(ctx, sc.RemoteDrive, sc.RemoteItem); err != nil {
			e.logger.Warn("failed to delete shortcut delta token",
				slog.String("item_id", sc.ItemID),
				slog.String("error", err.Error()),
			)
		}

		if err := e.baseline.DeleteShortcut(ctx, sc.ItemID); err != nil {
			return fmt.Errorf("sync: deleting shortcut %s: %w", sc.ItemID, err)
		}
	}

	return nil
}

// scopeResult holds the observation output for a single shortcut scope.
// Delta tokens are collected and committed only after all scopes succeed,
// preventing partial token advancement on crash.
type scopeResult struct {
	events     []ChangeEvent
	deltaToken string // non-empty for delta-observed scopes
	shortcut   *Shortcut
}

// observeShortcutContent observes content for all active shortcuts concurrently.
// Delegates to observeShortcutContentFromList after loading shortcuts from the DB.
func (e *Engine) observeShortcutContent(ctx context.Context, bl *Baseline, collisions map[string]bool) ([]ChangeEvent, error) {
	shortcuts, err := e.baseline.ListShortcuts(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: listing shortcuts for observation: %w", err)
	}

	return e.observeShortcutContentFromList(ctx, shortcuts, bl, collisions)
}

// observeShortcutContentFromList is the list-threaded variant (B-333).
// Observes content for all active shortcuts concurrently (up to
// maxShortcutConcurrency). Delta tokens are deferred until all scopes
// complete, ensuring atomicity.
func (e *Engine) observeShortcutContentFromList(
	ctx context.Context, shortcuts []Shortcut, bl *Baseline, collisions map[string]bool,
) ([]ChangeEvent, error) {
	if len(shortcuts) == 0 {
		return nil, nil
	}

	results := make([]scopeResult, len(shortcuts))

	var mu stdsync.Mutex

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

			result, scErr := e.observeSingleShortcut(gCtx, sc, bl)
			if scErr != nil {
				e.logger.Warn("shortcut observation failed, skipping",
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
		return nil, fmt.Errorf("sync: shortcut observation: %w", err)
	}

	// Commit all delta tokens after all scopes are done.
	var allEvents []ChangeEvent

	for i := range results {
		allEvents = append(allEvents, results[i].events...)

		if results[i].deltaToken != "" {
			sc := results[i].shortcut
			if err := e.baseline.CommitDeltaToken(ctx, results[i].deltaToken, sc.RemoteDrive, sc.RemoteItem, sc.RemoteDrive); err != nil {
				e.logger.Warn("failed to commit shortcut delta token",
					slog.String("item_id", sc.ItemID),
					slog.String("error", err.Error()),
				)
			}
		}
	}

	if len(allEvents) > 0 {
		e.logger.Info("shortcut observation complete",
			slog.Int("shortcuts", len(shortcuts)),
			slog.Int("events", len(allEvents)),
		)
	}

	return allEvents, nil
}

// observeSingleShortcut observes content for one shortcut scope.
// Returns a scopeResult with events and an optional delta token to commit.
func (e *Engine) observeSingleShortcut(ctx context.Context, sc *Shortcut, bl *Baseline) (scopeResult, error) {
	remoteDriveID := driveid.New(sc.RemoteDrive)

	switch sc.Observation {
	case ObservationDelta:
		return e.observeShortcutDelta(ctx, sc, remoteDriveID, bl)
	default:
		// B-335: ObservationEnumerate and ObservationUnknown both use enumerate.
		return e.observeShortcutEnumerate(ctx, sc, remoteDriveID, bl)
	}
}

// observeShortcutDelta uses folder-scoped delta to observe shortcut content.
// Returns the events and the new delta token (not committed — caller handles that).
func (e *Engine) observeShortcutDelta(
	ctx context.Context, sc *Shortcut, remoteDriveID driveid.ID, bl *Baseline,
) (scopeResult, error) {
	if e.folderDelta == nil {
		return scopeResult{}, fmt.Errorf("sync: folder delta not available for shortcut %s", sc.ItemID)
	}

	savedToken, err := e.baseline.GetDeltaToken(ctx, sc.RemoteDrive, sc.RemoteItem)
	if err != nil {
		return scopeResult{}, fmt.Errorf("sync: getting shortcut delta token: %w", err)
	}

	items, newToken, err := e.folderDelta.DeltaFolderAll(ctx, remoteDriveID, sc.RemoteItem, savedToken)
	if err != nil {
		if errors.Is(err, graph.ErrGone) {
			e.logger.Warn("shortcut delta token expired, performing full resync",
				slog.String("item_id", sc.ItemID),
			)

			items, newToken, err = e.folderDelta.DeltaFolderAll(ctx, remoteDriveID, sc.RemoteItem, "")
			if err != nil {
				return scopeResult{}, fmt.Errorf("sync: shortcut full resync: %w", err)
			}
		} else {
			return scopeResult{}, fmt.Errorf("sync: shortcut delta: %w", err)
		}
	}

	events := shortcutItemsToEventsWithLog(items, sc, remoteDriveID, bl, e.logger)

	return scopeResult{
		events:     events,
		deltaToken: newToken,
		shortcut:   sc,
	}, nil
}

// observeShortcutEnumerate uses recursive listing to observe shortcut content.
func (e *Engine) observeShortcutEnumerate(
	ctx context.Context, sc *Shortcut, remoteDriveID driveid.ID, bl *Baseline,
) (scopeResult, error) {
	if e.recursiveLister == nil {
		return scopeResult{}, fmt.Errorf("sync: recursive lister not available for shortcut %s", sc.ItemID)
	}

	items, err := e.recursiveLister.ListChildrenRecursive(ctx, remoteDriveID, sc.RemoteItem)
	if err != nil {
		return scopeResult{}, fmt.Errorf("sync: shortcut enumerate: %w", err)
	}

	events := shortcutItemsToEventsWithLog(items, sc, remoteDriveID, bl, e.logger)

	// Detect deletions: items in baseline under this scope but not in enumeration.
	orphans := detectShortcutOrphans(sc, remoteDriveID, items, bl)
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
func (e *Engine) reconcileShortcutScopes(ctx context.Context, bl *Baseline) ([]ChangeEvent, error) {
	if e.folderDelta == nil && e.recursiveLister == nil {
		return nil, nil
	}

	shortcuts, err := e.baseline.ListShortcuts(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: listing shortcuts for reconciliation: %w", err)
	}

	if len(shortcuts) == 0 {
		return nil, nil
	}

	collisions := e.detectShortcutCollisions(ctx, bl)
	results := make([]scopeResult, len(shortcuts))

	var mu stdsync.Mutex

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(maxShortcutConcurrency)

	for i := range shortcuts {
		sc := &shortcuts[i]

		g.Go(func() error {
			mu.Lock()
			results[i].shortcut = sc
			mu.Unlock()

			if collisions[sc.ItemID] {
				return nil
			}

			remoteDriveID := driveid.New(sc.RemoteDrive)

			var result scopeResult
			var scErr error

			switch sc.Observation {
			case ObservationDelta:
				result, scErr = e.reconcileShortcutDelta(gCtx, sc, remoteDriveID, bl)
			default:
				result, scErr = e.observeShortcutEnumerate(gCtx, sc, remoteDriveID, bl)
			}

			if scErr != nil {
				e.logger.Warn("shortcut reconciliation failed, skipping",
					slog.String("item_id", sc.ItemID),
					slog.String("error", scErr.Error()),
				)

				return nil
			}

			mu.Lock()
			results[i].events = result.events
			results[i].deltaToken = result.deltaToken
			mu.Unlock()

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("sync: shortcut reconciliation: %w", err)
	}

	var allEvents []ChangeEvent

	for i := range results {
		allEvents = append(allEvents, results[i].events...)

		if results[i].deltaToken != "" {
			sc := results[i].shortcut
			if err := e.baseline.CommitDeltaToken(ctx, results[i].deltaToken, sc.RemoteDrive, sc.RemoteItem, sc.RemoteDrive); err != nil {
				e.logger.Warn("failed to commit reconciliation delta token",
					slog.String("item_id", sc.ItemID),
					slog.String("error", err.Error()),
				)
			}
		}
	}

	return allEvents, nil
}

// reconcileShortcutDelta performs a full delta enumeration for a shortcut
// by using an empty token. This enumerates all items via delta and detects
// orphans that may have been missed by incremental delta.
func (e *Engine) reconcileShortcutDelta(
	ctx context.Context, sc *Shortcut, remoteDriveID driveid.ID, bl *Baseline,
) (scopeResult, error) {
	if e.folderDelta == nil {
		return scopeResult{}, fmt.Errorf("sync: folder delta not available for shortcut %s", sc.ItemID)
	}

	items, newToken, err := e.folderDelta.DeltaFolderAll(ctx, remoteDriveID, sc.RemoteItem, "")
	if err != nil {
		return scopeResult{}, fmt.Errorf("sync: shortcut full reconciliation delta: %w", err)
	}

	events := shortcutItemsToEventsWithLog(items, sc, remoteDriveID, bl, e.logger)
	orphans := detectShortcutOrphans(sc, remoteDriveID, items, bl)
	events = append(events, orphans...)

	return scopeResult{
		events:     events,
		deltaToken: newToken,
		shortcut:   sc,
	}, nil
}
