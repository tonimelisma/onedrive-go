package sync

type shortcutRootReleaseCleanupPlan struct {
	Records []ShortcutRootRecord
	Changed bool
	Err     error
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
		next := shortcutRootCleanupBlocked(normalized, cleanupErr)
		return shortcutRootReleaseCleanupPlan{
			Records: []ShortcutRootRecord{next},
			Changed: !shortcutRootRecordsEqual(normalized, next),
			Err:     cleanupErr,
		}
	}
	cleanupPending := shortcutRootChildCleanupPending(normalized)
	nextRecords := []ShortcutRootRecord{cleanupPending}
	if normalized.Waiting != nil {
		nextRecords = append(nextRecords, shortcutRootRecordFromReplacement(normalized.NamespaceID, *normalized.Waiting))
	}
	return shortcutRootReleaseCleanupPlan{
		Records: nextRecords,
		Changed: true,
	}
}
