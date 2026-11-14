package sync

import (
	"cmp"
	"fmt"
)

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
	// KeptNotOnOneDrive, KeptNotUploaded, KeptNotWritable and KeptNotRemoved
	// are what the release of the tree kept (Report.KeptNotOnOneDrive,
	// Report.KeptNotUploaded, Report.KeptNotWritable, Report.KeptNotRemoved).
	KeptNotOnOneDrive KeptEntries
	KeptNotUploaded   KeptEntries
	KeptNotWritable   KeptEntries
	KeptNotRemoved    KeptEntries
}

// Any reports whether anything holds the local tree.
func (h *LocalTreeHolds) Any() bool {
	return h.NoReport || h.DryRun || h.Left.Any() || h.HeldLocalDeletes > 0 || h.SetAsideFiles > 0 ||
		h.KeptNotOnOneDrive.Count > 0 || h.KeptNotUploaded.Count > 0 || h.KeptNotWritable.Count > 0 ||
		h.KeptNotRemoved.Count > 0
}

// Reasons says what holds the tree, one sentence per part that holds it, in
// a fixed order: no report, a dry run, the work the run left by kind, held
// deletes of local files, set-aside files, and what the release kept
// (keptReasons). The zero value has none. It is the one place these
// sentences are made: a removed shared folder's final drain names them in its
// blocked detail and its notice.
func (h *LocalTreeHolds) Reasons() []string {
	var reasons []string
	if h.NoReport {
		reasons = append(reasons, "the final sync produced no report")
	}
	if h.DryRun {
		reasons = append(reasons, "the final sync was a dry run")
	}
	if h.Left.Failed > 0 {
		reasons = append(reasons, fmt.Sprintf("%d failed", h.Left.Failed))
	}
	if h.Left.WillRetry > 0 {
		reasons = append(reasons, fmt.Sprintf("%d to be retried on the next sync", h.Left.WillRetry))
	}
	if h.Left.StillOpen > 0 {
		reasons = append(reasons, fmt.Sprintf("%d still not synced", h.Left.StillOpen))
	}
	if h.HeldLocalDeletes > 0 {
		reasons = append(reasons, fmt.Sprintf("%d local %s held for confirmation",
			h.HeldLocalDeletes, countNoun(h.HeldLocalDeletes, "delete", "deletes")))
	}
	if h.SetAsideFiles > 0 {
		reasons = append(reasons, fmt.Sprintf("%d %s set aside by an interrupted removal",
			h.SetAsideFiles, countNoun(h.SetAsideFiles, "file", "files")))
	}

	return append(reasons, h.keptReasons()...)
}

// keptReasons says, one sentence per kind, what the release of the tree kept
// and what the user can do about it. The sentence for content sync does not
// manage is the one a folder deleted on OneDrive gives when it cannot be
// removed (folderNotEmptyReason).
func (h *LocalTreeHolds) keptReasons() []string {
	var reasons []string
	if h.KeptNotOnOneDrive.Count > 0 {
		reasons = append(reasons, folderNotEmptyReason(h.KeptNotOnOneDrive.Count, h.KeptNotOnOneDrive.Examples))
	}
	if h.KeptNotUploaded.Count > 0 {
		reasons = append(reasons, notUploadedReason(h.KeptNotUploaded.Count, h.KeptNotUploaded.Examples))
	}
	if h.KeptNotWritable.Count > 0 {
		reasons = append(reasons, notWritableReason(h.KeptNotWritable.Count, h.KeptNotWritable.Examples))
	}
	if h.KeptNotRemoved.Count > 0 {
		reasons = append(reasons, notRemovedReason(h.KeptNotRemoved.Count, h.KeptNotRemoved.Examples))
	}

	return reasons
}

// KeptEntries is what a release kept of one kind: how many entries, and the
// paths of the first few of them (as many as the folder-not-empty sentence
// names), relative to the sync folder; the sync folder itself is "".
type KeptEntries struct {
	Count    int
	Examples []string
}

// newKeptEntries counts paths and names the first few of them.
func newKeptEntries(paths []string) KeptEntries {
	if len(paths) == 0 {
		return KeptEntries{}
	}

	return KeptEntries{
		Count:    len(paths),
		Examples: append([]string(nil), paths[:min(len(paths), folderNotEmptyNamedLimit)]...),
	}
}

// notUploadedReason is the sentence for changes kept because the removed
// shared folder can no longer be read to upload them.
func notUploadedReason(count int, examples []string) string {
	return fmt.Sprintf("%d %s could not be uploaded because the shared folder can no longer be reached (%s); "+
		"copy what you need elsewhere, then delete them to finish",
		count, countNoun(count, "change", "changes"), namedWithMore(count, examples))
}

// notWritableReason is the sentence for synced folders the release could not
// remove anything from, because they cannot be written to.
func notWritableReason(count int, examples []string) string {
	named := make([]string, len(examples))
	for i, example := range examples {
		named[i] = cmp.Or(example, "the shared folder itself")
	}

	return fmt.Sprintf("%d %s cannot be written to (%s); make %s writable to finish",
		count, countNoun(count, "folder", "folders"), namedWithMore(count, named), countNoun(count, "it", "them"))
}

// notRemovedReason is the sentence for synced entries the release's local
// delete could not move to the trash or delete.
func notRemovedReason(count int, examples []string) string {
	pronoun := countNoun(count, "it", "them")

	return fmt.Sprintf("%d synced %s could not be moved to the trash or deleted (%s); "+
		"remove %s by hand or fix what keeps %s, then sync again",
		count, countNoun(count, "item", "items"), namedWithMore(count, examples), pronoun, pronoun)
}

// countNoun is singular for a count of one and plural otherwise.
func countNoun(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}

	return plural
}

// LocalTreeHolds answers whether the local tree this run synced is gone or
// may go, as a removed shared folder's local copy is once its final sync
// finished (R-2.4.8): it is when nothing holds it. A dry run holds it
// whatever it found, and so does a run that left work, held deletes of local
// files, left files an interrupted removal set aside, or released the tree
// and kept what the local delete rule does not or cannot remove. Held deletes
// of files on OneDrive do not: their local copies are already gone.
func (r *Report) LocalTreeHolds() *LocalTreeHolds {
	if r == nil {
		return &LocalTreeHolds{NoReport: true}
	}

	return &LocalTreeHolds{
		DryRun:            r.DryRun,
		Left:              r.LeftWork(),
		HeldLocalDeletes:  r.HeldLocalDeletes,
		SetAsideFiles:     r.InterruptedRemovals,
		KeptNotOnOneDrive: r.KeptNotOnOneDrive,
		KeptNotUploaded:   r.KeptNotUploaded,
		KeptNotWritable:   r.KeptNotWritable,
		KeptNotRemoved:    r.KeptNotRemoved,
	}
}
