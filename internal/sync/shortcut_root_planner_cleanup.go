package sync

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
	normalized := normalizeShortcutRootRecord(record)
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

// shortcutRootReleaseResult is what removing a released alias root found:
// the root is gone (removed now or already absent), it still holds
// something, or the removal failed for a reason no final drain can fix.
type shortcutRootReleaseResult struct {
	NotEmpty bool
	Err      error
}

// planShortcutRootReleaseCleanup is the deterministic core for the parent
// release-cleanup phase. The engine shell owns filesystem removal; this helper
// only translates that outcome into the next durable shortcut-root records.
func planShortcutRootReleaseCleanup(
	record *ShortcutRootRecord,
	result shortcutRootReleaseResult,
) shortcutRootReleaseCleanupPlan {
	if record == nil {
		return shortcutRootReleaseCleanupPlan{}
	}
	normalized := normalizeShortcutRootRecord(record)
	if !shortcutRootStateAwaitsReleaseCleanup(normalized.State) {
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
