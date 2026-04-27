package sync

//nolint:gocritic // ShortcutRootRecord is an immutable planner value at this boundary.
func planShortcutRootCleanupBlocked(record ShortcutRootRecord, err error) ShortcutRootRecord {
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

//nolint:gocritic // ShortcutRootRecord is an immutable planner value at this boundary.
func planShortcutRootChildCleanupPending(record ShortcutRootRecord) ShortcutRootRecord {
	record = normalizeShortcutRootRecord(record)
	record = plannedShortcutRootTransition(record,
		shortcutRootEventProjectionCleanupSucceeded,
		ShortcutRootStateRemovedChildCleanupPending,
		"",
	)
	record.BlockedDetail = ""
	record.ProtectedPaths = nil
	record.LocalRootIdentity = nil
	record.Waiting = nil
	return record
}

// planShortcutRootReleaseCleanup is the deterministic core for the parent
// release-cleanup phase. The engine shell owns filesystem removal; this helper
// only translates that outcome into the next durable shortcut-root records.
func planShortcutRootReleaseCleanup(
	record *ShortcutRootRecord,
	cleanupErr error,
) shortcutRootReleaseCleanupPlan {
	if record == nil {
		return shortcutRootReleaseCleanupPlan{}
	}
	normalized := normalizeShortcutRootRecord(*record)
	if !shortcutRootStateAwaitsReleaseCleanup(normalized.State) {
		return shortcutRootReleaseCleanupPlan{
			Records: []ShortcutRootRecord{normalized},
		}
	}
	if cleanupErr != nil {
		next := planShortcutRootCleanupBlocked(normalized, cleanupErr)
		return shortcutRootReleaseCleanupPlan{
			Records: []ShortcutRootRecord{next},
			Changed: !shortcutRootRecordsEqual(normalized, next),
			Err:     cleanupErr,
		}
	}
	cleanupPending := planShortcutRootChildCleanupPending(normalized)
	nextRecords := []ShortcutRootRecord{cleanupPending}
	if normalized.Waiting != nil {
		nextRecords = append(nextRecords, shortcutRootRecordFromReplacement(normalized.NamespaceID, *normalized.Waiting))
	}
	return shortcutRootReleaseCleanupPlan{
		Records: nextRecords,
		Changed: true,
	}
}
