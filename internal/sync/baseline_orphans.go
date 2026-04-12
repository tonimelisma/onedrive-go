package sync

import (
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// findBaselineOrphans identifies baseline entries that are not present in the
// seen set from a full enumeration and synthesizes remote delete events for them.
func findBaselineOrphans(bl *Baseline, seen map[driveid.ItemKey]struct{}, driveID driveid.ID, pathPrefix string) []ChangeEvent {
	var orphans []ChangeEvent

	bl.ForEachPath(func(path string, entry *BaselineEntry) {
		if entry.DriveID != driveID {
			return
		}
		if pathPrefix != "" && path != pathPrefix && !strings.HasPrefix(path, pathPrefix+"/") {
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
