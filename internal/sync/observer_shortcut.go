package sync

import (
	"log/slog"
	"path"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

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

// shortcutItemsToEvents converts graph.Items from a shortcut scope into
// ChangeEvents with paths prefixed by the shortcut's local path.
// Uses O(n) memoized path building via buildShortcutPathMap.
func shortcutItemsToEvents(
	items []graph.Item, sc *Shortcut, remoteDriveID driveid.ID, bl *Baseline,
) []ChangeEvent {
	return shortcutItemsToEventsWithLog(items, sc, remoteDriveID, bl, nil)
}

// shortcutItemsToEventsWithLog converts graph.Items from a shortcut scope
// into ChangeEvents, skipping nested shortcuts (items with RemoteItemID).
func shortcutItemsToEventsWithLog(
	items []graph.Item, sc *Shortcut, remoteDriveID driveid.ID, bl *Baseline,
	logger *slog.Logger,
) []ChangeEvent {
	pathMap := buildShortcutPathMap(items, sc.RemoteItem)

	var events []ChangeEvent

	for i := range items {
		item := &items[i]

		if item.IsRoot || item.ID == sc.RemoteItem {
			continue
		}

		// Skip nested shortcuts — items that themselves reference another drive.
		if item.RemoteItemID != "" {
			if logger != nil {
				logger.Warn("nested shortcut detected in observation, skipping",
					slog.String("item_id", item.ID),
					slog.String("name", item.Name),
					slog.String("parent_shortcut", sc.ItemID),
				)
			}

			continue
		}

		relPath := pathMap[item.ID]
		if relPath == "" {
			relPath = item.Name
		}

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

// buildShortcutPathMap builds a map of itemID → relative path within the
// shortcut scope in O(n) amortized time using memoization. Each item's path
// is resolved once and cached; subsequent lookups are O(1).
func buildShortcutPathMap(items []graph.Item, rootItemID string) map[string]string {
	type parentInfo struct {
		name     string
		parentID string
	}

	index := make(map[string]parentInfo, len(items))
	for i := range items {
		if items[i].IsRoot || items[i].ID == rootItemID {
			continue
		}

		index[items[i].ID] = parentInfo{
			name:     items[i].Name,
			parentID: items[i].ParentID,
		}
	}

	resolved := make(map[string]string, len(index))

	var resolve func(id string, depth int) string
	resolve = func(id string, depth int) string {
		if p, ok := resolved[id]; ok {
			return p
		}

		if depth > maxPathDepth {
			// Safety: break cycles or absurdly deep trees.
			return index[id].name
		}

		info, ok := index[id]
		if !ok {
			return ""
		}

		if info.parentID == "" || info.parentID == rootItemID {
			resolved[id] = info.name
			return info.name
		}

		parentPath := resolve(info.parentID, depth+1)
		full := parentPath + "/" + info.name

		if parentPath == "" {
			full = info.name
		}

		resolved[id] = full

		return full
	}

	for id := range index {
		resolve(id, 0)
	}

	return resolved
}

// mapShortcutPath prefixes a relative path within a shortcut scope with
// the shortcut's local path.
func mapShortcutPath(shortcutLocalPath, relPath string) string {
	return path.Join(shortcutLocalPath, relPath)
}

// detectShortcutOrphans finds baseline entries belonging to a shortcut scope
// that are no longer present in the full enumeration. Delegates to
// Baseline.FindOrphans with a path prefix filter for the shortcut's local path.
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

	return bl.FindOrphans(seen, remoteDriveID, sc.LocalPath)
}
