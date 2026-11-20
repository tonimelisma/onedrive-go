package sync

import (
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// findBaselineOrphans identifies baseline entries that are not present in the
// seen set from a full enumeration and synthesizes a deleted row for each.
func findBaselineOrphans(bl *Baseline, seen map[string]struct{}, driveID driveid.ID, pathPrefix string) []remoteItem {
	var orphans []remoteItem

	bl.ForEachPath(func(path string, entry *BaselineEntry) {
		if entry.DriveID != driveID {
			return
		}
		if pathPrefix != "" && path != pathPrefix && !strings.HasPrefix(path, pathPrefix+"/") {
			return
		}

		if _, ok := seen[entry.ItemID]; ok {
			return
		}

		orphans = append(orphans, remoteItem{observedItem: observedItem{
			Path:      entry.Path,
			ItemID:    entry.ItemID,
			DriveID:   entry.DriveID,
			ItemType:  entry.ItemType,
			IsDeleted: true,
		}})
	})

	return orphans
}

// findRemoteStateOrphans synthesizes a deleted row for each remote_state
// rows that a full enumeration no longer returns and that findBaselineOrphans
// did not already cover.
func findRemoteStateOrphans(
	rows []remoteStateRow,
	seen map[string]struct{},
	alreadyOrphaned []remoteItem,
	driveID driveid.ID,
) []remoteItem {
	covered := make(map[string]struct{}, len(alreadyOrphaned))
	for i := range alreadyOrphaned {
		covered[alreadyOrphaned[i].ItemID] = struct{}{}
	}

	var orphans []remoteItem
	for i := range rows {
		row := &rows[i]
		if !row.DriveID.IsZero() && row.DriveID != driveID {
			continue
		}
		if _, ok := seen[row.ItemID]; ok {
			continue
		}
		if _, ok := covered[row.ItemID]; ok {
			continue
		}

		orphans = append(orphans, remoteItem{observedItem: observedItem{
			Path:      row.Path,
			ItemID:    row.ItemID,
			DriveID:   driveID,
			ItemType:  row.ItemType,
			IsDeleted: true,
		}})
	}

	return orphans
}
