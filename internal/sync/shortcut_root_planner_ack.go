package sync

import "slices"

func planShortcutRootDrainReleasePending(
	current []ShortcutRootRecord,
	ack ShortcutChildDrainAck,
) shortcutRootDrainAckPlan {
	records := make([]ShortcutRootRecord, 0, len(current))
	changed := false
	for i := range current {
		record := normalizeShortcutRootRecord(&current[i])
		if record.BindingItemID != ack.Ref.bindingItemID {
			records = append(records, record)
			continue
		}
		if !shortcutRootStateAwaitsFinalDrainAck(record.State) {
			records = append(records, record)
			continue
		}
		next := plannedShortcutRootTransition(&record,
			shortcutRootEventChildFinalDrainClean,
			ShortcutRootStateRemovedReleasePending,
			"",
		)
		records = append(records, next)
		changed = changed || !shortcutRootRecordsEqual(&record, &next)
	}
	slices.SortFunc(records, func(a, b ShortcutRootRecord) int {
		if a.RelativeLocalPath == b.RelativeLocalPath {
			return compareString(a.BindingItemID, b.BindingItemID)
		}
		return compareString(a.RelativeLocalPath, b.RelativeLocalPath)
	})
	return shortcutRootDrainAckPlan{Records: records, Changed: changed}
}

func planShortcutRootArtifactCleanupAck(
	current []ShortcutRootRecord,
	ack ShortcutChildArtifactCleanupAck,
) shortcutRootArtifactCleanupAckPlan {
	records := make([]ShortcutRootRecord, 0, len(current))
	changed := false
	for i := range current {
		record := normalizeShortcutRootRecord(&current[i])
		if record.BindingItemID == ack.Ref.bindingItemID &&
			record.State == ShortcutRootStateRemovedChildCleanupPending {
			changed = true
			continue
		}
		records = append(records, record)
	}
	return shortcutRootArtifactCleanupAckPlan{Records: records, Changed: changed}
}
