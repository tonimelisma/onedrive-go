package sync

import (
	"path"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
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
