package syncshortcut

func planShortcutRootCleanupBlocked(record *ShortcutRootRecord, err error) ShortcutRootRecord {
	if record == nil {
		return ShortcutRootRecord{}
	}
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	return plannedShortcutRootTransition(record,
		shortcutRootEventProjectionCleanupFailed,
		ShortcutRootStateRemovedCleanupBlocked,
		detail,
	)
}

func planShortcutRootChildCleanupPending(record *ShortcutRootRecord) ShortcutRootRecord {
	if record == nil {
		return ShortcutRootRecord{}
	}
	normalized := NormalizeShortcutRootRecord(record)
	normalized = plannedShortcutRootTransition(&normalized,
		shortcutRootEventProjectionCleanupSucceeded,
		ShortcutRootStateRemovedChildCleanupPending,
		"",
	)
	normalized.BlockedDetail = ""
	normalized.ProtectedPaths = nil
	normalized.LocalRootIdentity = nil
	normalized.Waiting = nil
	return normalized
}

// PlanExcludedRetiringShortcutRoot is the next records of a removed shortcut
// that waits for its final drain while the parent's content filter excludes
// its path. An excluded shortcut has no child run (the child work publication
// leaves it out), so no drain would ever acknowledge it and the record would
// wait for ever. Nothing of it is synced, so there is nothing to drain: its
// sync data goes to the child cleanup, and its local folder is left exactly
// as it is, since an excluded path is never touched (no release, which would
// remove the folder). A same-path replacement it held back takes its place.
// A record in any other state comes back as it is.
func PlanExcludedRetiringShortcutRoot(record *ShortcutRootRecord) ([]ShortcutRootRecord, bool) {
	if record == nil {
		return nil, false
	}
	normalized := NormalizeShortcutRootRecord(record)
	if !shortcutRootStateAwaitsFinalDrainAck(normalized.State) || normalized.State == ShortcutRootStateRemovedCleanupBlocked {
		return []ShortcutRootRecord{normalized}, false
	}
	waiting := normalized.Waiting
	next := plannedShortcutRootTransition(&normalized,
		shortcutRootEventExcludedFinalDrainSkipped,
		ShortcutRootStateRemovedChildCleanupPending,
		"",
	)
	next.BlockedDetail = ""
	next.ProtectedPaths = nil
	next.LocalRootIdentity = nil
	next.Waiting = nil
	records := []ShortcutRootRecord{next}
	if waiting != nil {
		records = append(records, shortcutRootRecordFromReplacement(normalized.NamespaceID, *waiting))
	}

	return records, true
}

// planShortcutRootFinalDrainAgain sends a released alias root that still holds
// something back to its final drain: the child, which owns the content, runs
// again and releases or reports what is there. A record a same-path
// replacement waits behind goes back to waiting.
func planShortcutRootFinalDrainAgain(record *ShortcutRootRecord) ShortcutRootRecord {
	if record == nil {
		return ShortcutRootRecord{}
	}
	target := ShortcutRootStateRemovedFinalDrain
	if record.Waiting != nil {
		target = ShortcutRootStateSamePathReplacementWaiting
	}
	return plannedShortcutRootTransition(record, shortcutRootEventProjectionNotEmpty, target, "")
}

// ShortcutRootReleaseResult is what removing a released alias root found:
// the root is gone (removed now or already absent), it still holds
// something, or the removal failed for a reason no final drain can fix.
type ShortcutRootReleaseResult struct {
	NotEmpty bool
	Err      error
}

// PlanShortcutRootReleaseCleanup is the deterministic core for the parent
// release-cleanup phase. The engine shell owns filesystem removal; this helper
// only translates that outcome into the next durable shortcut-root records.
func PlanShortcutRootReleaseCleanup(
	record *ShortcutRootRecord,
	result ShortcutRootReleaseResult,
) shortcutRootReleaseCleanupPlan {
	if record == nil {
		return shortcutRootReleaseCleanupPlan{}
	}
	normalized := NormalizeShortcutRootRecord(record)
	if !ShortcutRootStateAwaitsReleaseCleanup(normalized.State) {
		return shortcutRootReleaseCleanupPlan{
			Records: []ShortcutRootRecord{normalized},
		}
	}
	if result.Err != nil {
		next := planShortcutRootCleanupBlocked(&normalized, result.Err)
		return shortcutRootReleaseCleanupPlan{
			Records: []ShortcutRootRecord{next},
			Changed: !shortcutRootRecordsEqual(&normalized, &next),
			Err:     result.Err,
		}
	}
	if result.NotEmpty {
		next := planShortcutRootFinalDrainAgain(&normalized)
		return shortcutRootReleaseCleanupPlan{
			Records: []ShortcutRootRecord{next},
			Changed: !shortcutRootRecordsEqual(&normalized, &next),
		}
	}
	cleanupPending := planShortcutRootChildCleanupPending(&normalized)
	nextRecords := []ShortcutRootRecord{cleanupPending}
	if normalized.Waiting != nil {
		nextRecords = append(nextRecords, shortcutRootRecordFromReplacement(normalized.NamespaceID, *normalized.Waiting))
	}
	return shortcutRootReleaseCleanupPlan{
		Records: nextRecords,
		Changed: true,
	}
}
