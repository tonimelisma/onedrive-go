package sync

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

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

	// Step 2: Handle removed shortcuts.
	if err := e.handleRemovedShortcuts(ctx, removedShortcutIDs); err != nil {
		return nil, err
	}

	// Step 3: Register/update shortcuts from shortcut events.
	if err := e.registerShortcuts(ctx, shortcutEvents); err != nil {
		return nil, err
	}

	if dryRun {
		return nil, nil
	}

	// Step 4: Observe content for all active shortcuts.
	return e.observeShortcutContent(ctx, bl)
}

// filterOutShortcuts removes ChangeShortcut events from a slice — they are
// consumed by processShortcuts and should not enter the planner as regular events.
func filterOutShortcuts(events []ChangeEvent) []ChangeEvent {
	n := 0

	for i := range events {
		if events[i].Type != ChangeShortcut {
			events[n] = events[i]
			n++
		}
	}

	return events[:n]
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

// observeShortcutContent observes content for all active shortcuts.
func (e *Engine) observeShortcutContent(ctx context.Context, bl *Baseline) ([]ChangeEvent, error) {
	shortcuts, err := e.baseline.ListShortcuts(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: listing shortcuts for observation: %w", err)
	}

	var allEvents []ChangeEvent

	for i := range shortcuts {
		sc := &shortcuts[i]

		events, err := e.observeSingleShortcut(ctx, sc, bl)
		if err != nil {
			e.logger.Warn("shortcut observation failed, skipping",
				slog.String("item_id", sc.ItemID),
				slog.String("remote_drive", sc.RemoteDrive),
				slog.String("error", err.Error()),
			)

			continue
		}

		allEvents = append(allEvents, events...)
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
func (e *Engine) observeSingleShortcut(ctx context.Context, sc *Shortcut, bl *Baseline) ([]ChangeEvent, error) {
	remoteDriveID := driveid.New(sc.RemoteDrive)

	switch sc.Observation {
	case ObservationDelta:
		return e.observeShortcutDelta(ctx, sc, remoteDriveID, bl)
	case ObservationEnumerate:
		return e.observeShortcutEnumerate(ctx, sc, remoteDriveID, bl)
	default:
		return e.observeShortcutEnumerate(ctx, sc, remoteDriveID, bl)
	}
}

// observeShortcutDelta uses folder-scoped delta to observe shortcut content.
func (e *Engine) observeShortcutDelta(
	ctx context.Context, sc *Shortcut, remoteDriveID driveid.ID, bl *Baseline,
) ([]ChangeEvent, error) {
	if e.folderDelta == nil {
		return nil, fmt.Errorf("sync: folder delta not available for shortcut %s", sc.ItemID)
	}

	savedToken, err := e.baseline.GetDeltaToken(ctx, sc.RemoteDrive, sc.RemoteItem)
	if err != nil {
		return nil, fmt.Errorf("sync: getting shortcut delta token: %w", err)
	}

	items, newToken, err := e.folderDelta.DeltaFolderAll(ctx, remoteDriveID, sc.RemoteItem, savedToken)
	if err != nil {
		if strings.Contains(err.Error(), "410") {
			e.logger.Warn("shortcut delta token expired, performing full resync",
				slog.String("item_id", sc.ItemID),
			)

			items, newToken, err = e.folderDelta.DeltaFolderAll(ctx, remoteDriveID, sc.RemoteItem, "")
			if err != nil {
				return nil, fmt.Errorf("sync: shortcut full resync: %w", err)
			}
		} else {
			return nil, fmt.Errorf("sync: shortcut delta: %w", err)
		}
	}

	events := shortcutItemsToEvents(items, sc, remoteDriveID, bl)

	// Commit delta token for this scope.
	if newToken != "" {
		if err := e.baseline.CommitDeltaToken(ctx, newToken, sc.RemoteDrive, sc.RemoteItem, sc.RemoteDrive); err != nil {
			return nil, fmt.Errorf("sync: saving shortcut delta token: %w", err)
		}
	}

	return events, nil
}

// observeShortcutEnumerate uses recursive listing to observe shortcut content.
func (e *Engine) observeShortcutEnumerate(
	ctx context.Context, sc *Shortcut, remoteDriveID driveid.ID, bl *Baseline,
) ([]ChangeEvent, error) {
	if e.recursiveLister == nil {
		return nil, fmt.Errorf("sync: recursive lister not available for shortcut %s", sc.ItemID)
	}

	items, err := e.recursiveLister.ListChildrenRecursive(ctx, remoteDriveID, sc.RemoteItem)
	if err != nil {
		return nil, fmt.Errorf("sync: shortcut enumerate: %w", err)
	}

	events := shortcutItemsToEvents(items, sc, remoteDriveID, bl)

	// Detect deletions: items in baseline under this scope but not in enumeration.
	orphans := detectShortcutOrphans(sc, remoteDriveID, items, bl)
	events = append(events, orphans...)

	return events, nil
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

	var allEvents []ChangeEvent

	for i := range shortcuts {
		sc := &shortcuts[i]
		remoteDriveID := driveid.New(sc.RemoteDrive)

		var events []ChangeEvent

		var scErr error

		switch sc.Observation {
		case ObservationDelta:
			// Full delta with empty token = enumerate all items via delta.
			events, scErr = e.reconcileShortcutDelta(ctx, sc, remoteDriveID, bl)
		default:
			// Enumerate: same as normal observation (already a full enum).
			events, scErr = e.observeShortcutEnumerate(ctx, sc, remoteDriveID, bl)
		}

		if scErr != nil {
			e.logger.Warn("shortcut reconciliation failed, skipping",
				slog.String("item_id", sc.ItemID),
				slog.String("error", scErr.Error()),
			)

			continue
		}

		allEvents = append(allEvents, events...)
	}

	return allEvents, nil
}

// reconcileShortcutDelta performs a full delta enumeration for a shortcut
// by using an empty token. This enumerates all items via delta and detects
// orphans that may have been missed by incremental delta.
func (e *Engine) reconcileShortcutDelta(
	ctx context.Context, sc *Shortcut, remoteDriveID driveid.ID, bl *Baseline,
) ([]ChangeEvent, error) {
	if e.folderDelta == nil {
		return nil, fmt.Errorf("sync: folder delta not available for shortcut %s", sc.ItemID)
	}

	items, newToken, err := e.folderDelta.DeltaFolderAll(ctx, remoteDriveID, sc.RemoteItem, "")
	if err != nil {
		return nil, fmt.Errorf("sync: shortcut full reconciliation delta: %w", err)
	}

	events := shortcutItemsToEvents(items, sc, remoteDriveID, bl)
	orphans := detectShortcutOrphans(sc, remoteDriveID, items, bl)
	events = append(events, orphans...)

	// Commit new delta token for this scope.
	if newToken != "" {
		if err := e.baseline.CommitDeltaToken(ctx, newToken, sc.RemoteDrive, sc.RemoteItem, sc.RemoteDrive); err != nil {
			return nil, fmt.Errorf("sync: saving reconciliation delta token: %w", err)
		}
	}

	return events, nil
}

// shortcutItemsToEvents converts graph.Items from a shortcut scope into
// ChangeEvents with paths prefixed by the shortcut's local path.
// Builds relative paths by walking parent chains within the item batch.
func shortcutItemsToEvents(
	items []graph.Item, sc *Shortcut, remoteDriveID driveid.ID, bl *Baseline,
) []ChangeEvent {
	// Build parent map for path construction.
	parentMap := make(map[string]shortcutParent, len(items))
	for i := range items {
		parentMap[items[i].ID] = shortcutParent{
			name:     items[i].Name,
			parentID: items[i].ParentID,
		}
	}

	var events []ChangeEvent

	for i := range items {
		item := &items[i]

		if item.IsRoot {
			continue
		}

		// Build relative path within the shortcut scope.
		relPath := buildShortcutRelPath(item, parentMap, sc.RemoteItem)
		mappedPath := mapShortcutPath(sc.LocalPath, relPath)

		hash := driveops.SelectHash(item)
		itemDriveID := remoteDriveID

		if !item.DriveID.IsZero() {
			itemDriveID = item.DriveID
		}

		baselineKey := driveid.NewItemKey(itemDriveID, item.ID)
		existing, _ := bl.GetByID(baselineKey)

		ev := ChangeEvent{
			Source:   SourceRemote,
			Path:     mappedPath,
			ItemID:   item.ID,
			ParentID: item.ParentID,
			DriveID:  itemDriveID,
			ItemType: classifyItemType(item),
			Name:     item.Name,
			Size:     item.Size,
			Hash:     hash,
			Mtime:    toUnixNano(item.ModifiedAt),
			ETag:     item.ETag,
			CTag:     item.CTag,
		}

		switch {
		case item.IsDeleted:
			ev.Type = ChangeDelete
			ev.IsDeleted = true

			if existing != nil {
				ev.Path = existing.Path
			}
		case existing != nil:
			ev.Type = ChangeModify
		default:
			ev.Type = ChangeCreate
		}

		events = append(events, ev)
	}

	return events
}

// shortcutParent tracks an item's name and parent for path construction.
type shortcutParent struct {
	name     string
	parentID string
}

// buildShortcutRelPath constructs the relative path of an item within a
// shortcut scope by walking the parent chain. Stops at the source folder
// (rootItemID) or when the chain breaks.
func buildShortcutRelPath(item *graph.Item, parentMap map[string]shortcutParent, rootItemID string) string {
	segments := []string{item.Name}
	parentID := item.ParentID

	for depth := 0; depth < maxPathDepth; depth++ {
		if parentID == "" || parentID == rootItemID {
			break
		}

		p, ok := parentMap[parentID]
		if !ok {
			break
		}

		segments = append(segments, p.name)
		parentID = p.parentID
	}

	// Reverse to get root-first order.
	for i, j := 0, len(segments)-1; i < j; i, j = i+1, j-1 {
		segments[i], segments[j] = segments[j], segments[i]
	}

	return strings.Join(segments, "/")
}

// mapShortcutPath prefixes a relative path within a shortcut scope with
// the shortcut's local path.
func mapShortcutPath(shortcutLocalPath, relPath string) string {
	return path.Join(shortcutLocalPath, relPath)
}

// detectShortcutOrphans finds baseline entries belonging to a shortcut scope
// that are no longer present in the full enumeration.
func detectShortcutOrphans(
	sc *Shortcut, remoteDriveID driveid.ID, items []graph.Item, bl *Baseline,
) []ChangeEvent {
	seen := make(map[driveid.ItemKey]struct{}, len(items))
	for i := range items {
		itemDriveID := remoteDriveID

		if !items[i].DriveID.IsZero() {
			itemDriveID = items[i].DriveID
		}

		seen[driveid.NewItemKey(itemDriveID, items[i].ID)] = struct{}{}
	}

	prefix := sc.LocalPath + "/"
	var orphans []ChangeEvent

	bl.ForEachPath(func(p string, entry *BaselineEntry) {
		if !strings.HasPrefix(p, prefix) && p != sc.LocalPath {
			return
		}

		if entry.DriveID != remoteDriveID {
			return
		}

		key := driveid.NewItemKey(entry.DriveID, entry.ItemID)
		if _, ok := seen[key]; ok {
			return
		}

		orphans = append(orphans, ChangeEvent{
			Source:    SourceRemote,
			Type:      ChangeDelete,
			Path:      entry.Path,
			ItemID:    entry.ItemID,
			DriveID:   entry.DriveID,
			ItemType:  entry.ItemType,
			IsDeleted: true,
		})
	})

	return orphans
}
