package sync

// LeftWork is what a one-shot run left for a later run or the user, as its
// last pass ended (R-6.6.28): each file once, in one of three counts. A file
// an earlier pass failed and a later pass of the same run finished, or no
// longer needed, is in none of them.
type LeftWork struct {
	// Failed is the last pass's failed actions the engine does not retry by
	// itself.
	Failed int
	// WillRetry is the last pass's failures kept for retry plus the work
	// it left for the next sync (Report.LeftForNextSync).
	WillRetry int
	// StillOpen is the observation issues that keep a file unsynced
	// (Report.UnsyncedIssues).
	StillOpen int
}

// Total is how many files the run left.
func (w LeftWork) Total() int {
	return w.Failed + w.WillRetry + w.StillOpen
}

// Any reports whether the run left anything, which fails a one-shot sync.
func (w LeftWork) Any() bool {
	return w.Total() > 0
}

// LeftWork returns what the run left, read from the state its last pass
// ended in. It is the one place those counts are added up: the text and JSON
// summaries, the exit status and a removed shared folder's final drain all
// read it. A nil report and a dry run left nothing: they attempted nothing.
// Work a one-way mode held back and deletes held for confirmation are not
// work left.
func (r *Report) LeftWork() LeftWork {
	if r == nil || r.DryRun {
		return LeftWork{}
	}

	return LeftWork{
		Failed:    max(0, r.LastPassFailed-r.LastPassKeptForRetry),
		WillRetry: r.LastPassKeptForRetry + r.LeftForNextSync,
		StillOpen: r.UnsyncedIssues,
	}
}

// LocalTreeHolds is what keeps the local tree a run synced from being
// removed. The zero value holds nothing.
type LocalTreeHolds struct {
	// NoReport is set for a run that produced no report.
	NoReport bool
	// DryRun is set for a dry run, which synced nothing.
	DryRun bool
	// Left is what the run left (Report.LeftWork).
	Left LeftWork
	// HeldLocalDeletes is the deletes of local files the delete-safety hold
	// kept for confirmation: removing the tree would remove exactly those.
	HeldLocalDeletes int
	// SetAsideFiles is the files an interrupted removal set aside
	// (Report.InterruptedRemovals), which are the user's and never deleted.
	SetAsideFiles int
}

// Any reports whether anything holds the local tree.
func (h LocalTreeHolds) Any() bool {
	return h.NoReport || h.DryRun || h.Left.Any() || h.HeldLocalDeletes > 0 || h.SetAsideFiles > 0
}

// LocalTreeHolds answers whether the local tree this run synced may be
// removed, as a removed shared folder's local copy is once its final sync
// finished (R-2.4.8): it may when nothing holds it. A dry run holds it
// whatever it found, and so does a run that left work, held deletes of local
// files, or left files an interrupted removal set aside. Held deletes of
// files on OneDrive do not: their local copies are already gone.
func (r *Report) LocalTreeHolds() LocalTreeHolds {
	if r == nil {
		return LocalTreeHolds{NoReport: true}
	}

	return LocalTreeHolds{
		DryRun:           r.DryRun,
		Left:             r.LeftWork(),
		HeldLocalDeletes: r.HeldLocalDeletes,
		SetAsideFiles:    r.InterruptedRemovals,
	}
}
