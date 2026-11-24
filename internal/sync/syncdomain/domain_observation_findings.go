package syncdomain

// LocalObservationManagedIssueTypes lists the issue types a full local scan
// owns: its findings batch replaces the stored conditions of exactly these
// types, so a condition the scan does not report again is cleared. Every
// type a local scan reports is listed, because every full scan re-reports a
// finding whose cause is still there; a type left out keeps its condition
// after its cause is gone. Leaving one out on purpose needs its reason
// recorded where TestLocalObservationManagedIssueTypes_ManageEveryTypeALocalScanReports
// checks this list against the types the scanner's source names.
func LocalObservationManagedIssueTypes() []IssueType {
	return []IssueType{
		IssueInvalidFilename,
		IssuePathTooLong,
		IssueFileTooLarge,
		IssueCaseCollision,
		IssueLocalReadDenied,
		IssueHashError,
		// A boundary the scan proved last time (a synced path now holding a
		// symlink or FIFO, a mount point of another filesystem) is managed
		// too: when a later full scan finds the path representable again, the
		// condition clears and the path syncs again. A crossed mount point
		// that is not mounted is re-reported by every scan, so it stays until
		// the disk is back (R-6.2.14, R-6.2.15). Both mount types are managed
		// together, so a scan that reports one at a path replaces the other.
		IssueLocalUnsyncableReplacement,
		IssueLocalMountBoundary,
		IssueLocalMountMissing,
		// The leftover of an interrupted removal is reported by every scan
		// that finds it and by nothing else, so the scan that no longer finds
		// it (the user renamed it back or deleted it) clears the report. The
		// file itself is never touched (R-6.4.9).
		IssueLocalInterruptedRemoval,
		// What a removed shared folder's release kept is recorded by the
		// release after the run's last plan and by nothing else, so every
		// full scan clears it: a release that runs again records what is
		// still there, and a folder shared again keeps no stale report
		// (R-2.4.8).
		IssueRemovedFolderKept,
	}
}

func NewLocalObservationFindingsBatch() ObservationFindingsBatch {
	return ObservationFindingsBatch{
		ManagedIssueTypes: LocalObservationManagedIssueTypes(),
	}
}
