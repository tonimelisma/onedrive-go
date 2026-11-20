package sync

import (
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// FindBaselineOrphans identifies baseline entries that are not present in the
// seen set from a full enumeration and synthesizes a deleted row for each.
func FindBaselineOrphans(bl *Baseline, seen map[string]struct{}, driveID driveid.ID, pathPrefix string) []RemoteItem {
	var orphans []RemoteItem

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

		orphans = append(orphans, RemoteItem{ObservedItem: ObservedItem{
			Path:      entry.Path,
			ItemID:    entry.ItemID,
			DriveID:   entry.DriveID,
			ItemType:  entry.ItemType,
			IsDeleted: true,
		}})
	})

	return orphans
}

// FindRemoteStateOrphans synthesizes a deleted row for each remote_state
// rows that a full enumeration no longer returns and that FindBaselineOrphans
// did not already cover.
func FindRemoteStateOrphans(
	rows []RemoteStateRow,
	seen map[string]struct{},
	alreadyOrphaned []RemoteItem,
	driveID driveid.ID,
) []RemoteItem {
	covered := make(map[string]struct{}, len(alreadyOrphaned))
	for i := range alreadyOrphaned {
		covered[alreadyOrphaned[i].ItemID] = struct{}{}
	}

	var orphans []RemoteItem
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

		orphans = append(orphans, RemoteItem{ObservedItem: ObservedItem{
			Path:      row.Path,
			ItemID:    row.ItemID,
			DriveID:   driveID,
			ItemType:  row.ItemType,
			IsDeleted: true,
		}})
	}

	return orphans
}
