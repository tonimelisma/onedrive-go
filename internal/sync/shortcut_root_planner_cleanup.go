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
	normalized := normalizeShortcutRootRecord(record)
	if !shortcutRootStateAwaitsReleaseCleanup(normalized.State) {
		return shortcutRootReleaseCleanupPlan{
			Records: []ShortcutRootRecord{normalized},
		}
	}
	if cleanupErr != nil {
		next := planShortcutRootCleanupBlocked(&normalized, cleanupErr)
		return shortcutRootReleaseCleanupPlan{
			Records: []ShortcutRootRecord{next},
			Changed: !shortcutRootRecordsEqual(&normalized, &next),
			Err:     cleanupErr,
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
