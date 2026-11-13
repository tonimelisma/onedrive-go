package sync

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	slashpath "path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// This file is the release of a removed shared folder's local copy: the last
// step of the child engine's final drain (RunOptions.ReleaseLocalTree,
// R-2.4.8). The release obeys the rule of a folder deleted on OneDrive and
// nothing else: a synced file leaves only through the executor's local delete,
// which verifies its content against the baseline through the descriptor it
// removes and puts it in the trash; a synced folder leaves only once empty,
// with bundled junk disposable under the configured rule. What the rule does
// not or cannot remove stays, is counted and named in the report, and is
// recorded as a removed_folder_kept issue that `status` shows. The release
// deletes nothing on OneDrive and is not a user's delete, so the
// delete-safety hold neither judges nor counts it. The drain before it
// creates nothing new locally (leaveOutNewLocalItems), so an entry a release
// removed is never downloaded back by a later drain. The parent engine then
// removes the alias root only as an empty directory
// (shortcut_root_lifecycle.go).

// releaseBatchSize is how many baseline rows the release forgets before it
// checkpoints the store and removes their entries.
const releaseBatchSize = 256

// releaseLocalTree releases the tree this final drain synced, when its
// report lets it (releaseMayRun), and adds to the report what it removed and
// what it kept. While the shared folder can be read, a survey comes first:
// when it finds anything the rule would not remove, or a synced folder (the
// sync folder itself included) that cannot be written to, nothing is
// removed. Otherwise entries leave deepest
// first until the local delete refuses one, which ends the release there;
// what left before it stays gone, since the next drain creates nothing new
// locally (leaveOutNewLocalItems). A shared folder that can no longer be
// read gives up what matches the baseline, and what stays is named as not
// uploaded.
func (e *Engine) releaseLocalTree(ctx context.Context, report *Report) error {
	if report == nil || report.DryRun {
		return nil
	}
	issues, err := e.baseline.ListObservationIssues(ctx)
	if err != nil {
		return fmt.Errorf("sync: reading issues before releasing the local tree: %w", err)
	}
	unreachable := sharedFolderUnreadable(issues)
	if !releaseMayRun(report.LocalTreeHolds(), unreachable) {
		return nil
	}

	bl, err := e.baseline.Load(ctx)
	if err != nil {
		return fmt.Errorf("sync: loading baseline before releasing the local tree: %w", err)
	}
	if !unreachable {
		survey, surveyErr := e.surveyReleaseTree(bl)
		if surveyErr != nil {
			return surveyErr
		}
		if survey.holdsAnything() {
			return e.recordReleaseKept(ctx, report, survey, issues, false)
		}
	}

	released, refused, err := e.releaseBaselineRows(ctx, bl, !unreachable)
	report.Released = released
	if err != nil {
		return err
	}
	e.logger.Debug("released the local copy of a removed shared folder",
		slog.Int("released", released), slog.Bool("shared_folder_unreadable", unreachable))

	if bl, err = e.baseline.Load(ctx); err != nil {
		return fmt.Errorf("sync: loading baseline after releasing the local tree: %w", err)
	}
	survey, err := e.surveyReleaseTree(bl)
	if err != nil {
		return err
	}
	// An entry the delete refused for a reason the survey cannot see (content
	// that changed where no stat fact shows it, a trash that took nothing, an
	// immutable file) holds the tree too. A folder the delete refused only
	// because it still holds a kept entry is accounted for by that entry, so
	// it is neither filed nor warned about.
	e.reportReleaseRefusals(survey.addRefused(refused))
	if !survey.holdsAnything() {
		if survey, err = e.emptyReleasedRoot(bl); err != nil {
			return err
		}
	}

	return e.recordReleaseKept(ctx, report, survey, issues, unreachable)
}

// releaseMayRun reports whether the release runs after a final drain whose
// report holds the tree as holds says. It runs when nothing holds the tree.
// When the shared folder can no longer be read, the drain cannot finish, so
// the release also runs over the work that leaves, but still not over a dry
// run, a held delete of a local file or a file an interrupted removal set
// aside.
func releaseMayRun(holds *LocalTreeHolds, sharedFolderUnreachable bool) bool {
	if !holds.Any() {
		return true
	}

	return sharedFolderUnreachable && !holds.NoReport && !holds.DryRun &&
		holds.HeldLocalDeletes == 0 && holds.SetAsideFiles == 0
}

// sharedFolderUnreadable reports whether the store holds the remote read
// denial of the whole tree: OneDrive answered "not found" or "forbidden" for
// the folder this engine syncs, so nothing in it can be uploaded or read.
func sharedFolderUnreadable(issues []ObservationIssueRow) bool {
	root := sKPermRemoteRead("")
	for i := range issues {
		if issues[i].IssueType == issueRemoteReadDenied && issues[i].ScopeKey == root {
			return true
		}
	}

	return false
}

// releaseSurvey is what stands in the tree that the local delete rule would
// not or could not remove, by store path, the top-most entry of each:
// notOnOneDrive is content sync does not sync (a symbolic link, a special
// file, an entry the content filter hides or a name sync reserves), unsynced
// is a new or changed file or folder the drain did not see, notWritable is a
// synced folder, or the sync folder itself (""), that cannot be written to,
// so nothing in it can leave, and notRemoved is a synced entry the local
// delete refused for a reason no drain cures by itself (the trash took
// nothing, the file is immutable).
type releaseSurvey struct {
	notOnOneDrive []string
	unsynced      []string
	notWritable   []string
	notRemoved    []string
}

func (s *releaseSurvey) holdsAnything() bool {
	return len(s.notOnOneDrive)+len(s.unsynced)+len(s.notWritable)+len(s.notRemoved) > 0
}

// addRefused files the synced entries the release's delete refused that the
// survey does not account for: a file whose content the refusal proved
// changed is work for the next drain (unsynced), which uploads it; any other
// refusal (a trash that took nothing, an immutable file, a folder that would
// not go for a reason the survey cannot see) is an entry the release could
// not remove (notRemoved). It returns the refusals it filed as notRemoved.
func (s *releaseSurvey) addRefused(refused []releaseRefusal) []releaseRefusal {
	var notRemoved []releaseRefusal
	names := s.names()
	for i := range refused {
		refusal := &refused[i]
		if names.accountsFor(refusal.path) {
			continue
		}
		if refusal.contentChanged {
			s.unsynced = append(s.unsynced, refusal.path)
		} else {
			s.notRemoved = append(s.notRemoved, refusal.path)
			notRemoved = append(notRemoved, *refusal)
		}
		names.add(refusal.path)
	}
	slices.Sort(s.unsynced)
	slices.Sort(s.notRemoved)

	return notRemoved
}

// releaseSurveyNames tells, for many paths at a time, whether a survey
// accounts for a path: named holds every path the survey lists and every
// folder above one, unwritable every folder it lists as not writable.
type releaseSurveyNames struct {
	named      map[string]bool
	unwritable map[string]bool
}

// names builds the survey's lookup once.
func (s *releaseSurvey) names() releaseSurveyNames {
	names := releaseSurveyNames{named: map[string]bool{}, unwritable: map[string]bool{}}
	for _, kind := range [][]string{s.notOnOneDrive, s.unsynced, s.notWritable, s.notRemoved} {
		for _, listed := range kind {
			names.add(listed)
		}
	}
	for _, folder := range s.notWritable {
		names.unwritable[folder] = true
	}

	return names
}

// add names storedPath and every folder above it.
func (n releaseSurveyNames) add(storedPath string) {
	for p := storedPath; p != "" && p != "." && !n.named[p]; p = slashpath.Dir(p) {
		n.named[p] = true
	}
}

// accountsFor reports whether the survey names storedPath or an entry below
// it, or names a folder above it that cannot be written to.
func (n releaseSurveyNames) accountsFor(storedPath string) bool {
	if n.named[storedPath] || n.unwritable[""] {
		return true
	}
	for dir := slashpath.Dir(storedPath); dir != "." && dir != ""; dir = slashpath.Dir(dir) {
		if n.unwritable[dir] {
			return true
		}
	}

	return false
}

// addUnmatched files an entry the baseline does not hold as it is: a visible
// one is work for the next drain, a hidden one is content sync does not sync.
func (s *releaseSurvey) addUnmatched(filter contentFilter, storedPath string, itemType ItemType) {
	if filter.Visible(storedPath, itemType) {
		s.unsynced = append(s.unsynced, storedPath)
		return
	}
	s.notOnOneDrive = append(s.notOnOneDrive, storedPath)
}

// surveyReleaseTree walks the tree without following links and files every
// entry the local delete rule would not remove, and every folder it walks,
// the sync folder and each synced folder, that cannot be written to. A synced
// file counts as removable while its stat facts match the baseline
// (releaseFileLooksUnchanged); its content is verified by the delete itself.
func (e *Engine) surveyReleaseTree(bl *Baseline) (releaseSurvey, error) {
	walk := releaseSurveyWalk{
		engine:   e,
		exec:     newExecution(e.execCfg, bl),
		filter:   newContentFilter(e.contentFilter),
		baseline: bl,
	}
	if err := walk.dir("", ""); err != nil {
		return releaseSurvey{}, err
	}

	return walk.survey, nil
}

type releaseSurveyWalk struct {
	engine   *Engine
	exec     *executor
	filter   contentFilter
	baseline *Baseline
	survey   releaseSurvey
}

// dir files the entries of the folder stored at stored, spelled onDisk. A
// folder that cannot be written to is filed itself, and nothing below it:
// none of its entries could leave, so the folder stays whatever they are.
func (w *releaseSurveyWalk) dir(stored, onDisk string) error {
	entries, err := w.engine.syncTree.ReadDir(onDisk)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("sync: reading %s before releasing the local tree: %w", displayReleasePath(stored), err)
	}
	if !w.writable(onDisk) {
		w.survey.notWritable = append(w.survey.notWritable, stored)
		return nil
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return cmp.Compare(a.Name(), b.Name()) })
	for _, entry := range entries {
		if err := w.entry(stored, onDisk, entry.Name()); err != nil {
			return err
		}
	}

	return nil
}

func (w *releaseSurveyWalk) entry(stored, onDisk, name string) error {
	storedPath := slashpath.Join(stored, nfcNormalize(name))
	diskPath := filepath.Join(onDisk, name)
	info, err := w.engine.syncTree.Lstat(diskPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("sync: reading %s before releasing the local tree: %w", storedPath, err)
	}
	row, synced := w.baseline.GetByPath(storedPath)
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		// Never removed, followed or not: the release would remove only the
		// link, and a link to a folder holds rows it could not verify.
		w.survey.notOnOneDrive = append(w.survey.notOnOneDrive, storedPath)
	case info.IsDir():
		if synced && row.ItemType == ItemTypeFolder {
			return w.dir(storedPath, diskPath)
		}
		if w.exec.isDisposable(name, true) && w.exec.findNonDisposable(diskPath) == "" {
			return nil
		}
		w.survey.addUnmatched(w.filter, storedPath, ItemTypeFolder)
	case info.Mode().IsRegular():
		if synced && row.ItemType == ItemTypeFile {
			if !releaseFileLooksUnchanged(row, info) {
				w.survey.unsynced = append(w.survey.unsynced, storedPath)
			}
			return nil
		}
		if w.exec.isDisposable(name, false) {
			return nil
		}
		w.survey.addUnmatched(w.filter, storedPath, ItemTypeFile)
	default:
		w.survey.notOnOneDrive = append(w.survey.notOnOneDrive, storedPath)
	}

	return nil
}

// writable reports whether the release may remove entries from the folder
// spelled onDisk: the process may write to it and it is not on a read-only
// filesystem. A folder whose path cannot be resolved counts as not writable.
func (w *releaseSurveyWalk) writable(onDisk string) bool {
	abs, err := w.engine.syncTree.Abs(onDisk)
	if err != nil {
		return false
	}

	return isDirWritable(abs) && !isOnReadOnlyFilesystem(abs)
}

// releaseFileLooksUnchanged is the survey's stat test of a synced file. It
// compares the facts canReuseBaselineHash compares, size, modification time,
// change time and file identity, so an edit that put the modification time
// back, or another file renamed over this one, is found before anything is
// removed. A modification or change time the row lacks reads as changed: the
// next drain's scan records it (metadata drift). A size the row does not know
// is not compared: drift reads it as zero, so an empty file would never get
// one and the tree would stay held for good. Nor is an identity either side
// lacks: it proves nothing about the content, a filesystem that gives none
// would hold the tree for good, and a full scan backfills a row's missing
// identity anyway. The local delete still verifies the content. Nor is the
// racily-clean window applied: it guards trusting a stored hash, and the
// survey trusts none.
func releaseFileLooksUnchanged(row *BaselineEntry, info os.FileInfo) bool {
	if row.LocalSizeKnown && info.Size() != row.LocalSize {
		return false
	}
	if info.ModTime().UnixNano() != row.LocalMtime {
		return false
	}
	if ctime, hasCtime := synctree.ChangeTimeFromFileInfo(info); hasCtime && ctime != row.LocalCtime {
		return false
	}
	identity, hasIdentity := synctree.IdentityFromFileInfo(info)
	baseIdentity, baseHasIdentity := row.fileIdentity()

	return !hasIdentity || !baseHasIdentity || synctree.SameIdentity(identity, baseIdentity)
}

func displayReleasePath(stored string) string {
	if stored == "" {
		return "the sync folder"
	}

	return stored
}

// releaseBaselineRows takes the synced entries out of the tree, deepest
// first, by the local delete rule, and returns how many left and which
// synced entries the delete refused and kept. Each batch of rows leaves the
// baseline first, in the commit any local delete makes, and the store is
// checkpointed before an entry is touched: a crash or a lost write-ahead log
// can then leave an entry without its row, which the next run adopts or keeps
// as a conflict copy, but never a row without its entry, which it would read
// as deleted here and delete in the shared folder. An entry the delete
// refused gets its row back, without its change time when the delete proved
// its content changed (localContentChangedError), so the next scan reads it.
// With stopAtFirstRefusal (the shared folder can be read), the first refusal
// ends the release: the rest of its batch gets its rows back, in the same
// restore as the refused row, and no later batch starts. What left before it
// stays gone: the next drain creates nothing new locally.
func (e *Engine) releaseBaselineRows(ctx context.Context, bl *Baseline, stopAtFirstRefusal bool) (int, []releaseRefusal, error) {
	rows := releaseRowsDeepestFirst(bl)
	if len(rows) == 0 {
		return 0, nil, nil
	}
	localRows, err := e.baseline.ListLocalState(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("sync: reading local_state before releasing the local tree: %w", err)
	}
	localByPath := make(map[string]*localState, len(localRows))
	for i := range localRows {
		localByPath[localRows[i].Path] = localStateFromSnapshotRow(&localRows[i])
	}

	run := releaseRun{
		engine:             e,
		exec:               newExecution(e.execCfg, bl),
		localByPath:        localByPath,
		stopAtFirstRefusal: stopAtFirstRefusal,
	}
	for start := 0; start < len(rows) && !run.stopped; start += releaseBatchSize {
		if err := run.batch(ctx, rows[start:min(start+releaseBatchSize, len(rows))]); err != nil {
			return run.released, run.refused, err
		}
	}

	return run.released, run.refused, nil
}

// releaseRun is one release's removal phase: what it removed, which synced
// entries it kept and why, and whether a refusal ended it.
type releaseRun struct {
	engine             *Engine
	exec               *executor
	localByPath        map[string]*localState
	stopAtFirstRefusal bool
	released           int
	refused            []releaseRefusal
	stopped            bool
}

// errReleaseKeptWithoutCause stands in for the cause of an entry the local
// delete kept without an error and the survey does not account for.
var errReleaseKeptWithoutCause = errors.New("the local delete kept it and gave no reason")

// releaseRefusal is a synced entry the release's local delete kept: its path,
// the delete's error (nil for a folder kept for the ignored content in it),
// and whether that error proves the content changed
// (localContentChangedError), which the next drain uploads.
type releaseRefusal struct {
	path           string
	err            error
	contentChanged bool
}

// releaseRowsDeepestFirst copies the baseline's rows below the sync folder,
// children before their folder.
func releaseRowsDeepestFirst(bl *Baseline) []BaselineEntry {
	var rows []BaselineEntry
	bl.ForEachPath(func(_ string, entry *BaselineEntry) {
		if entry.Path == "" || entry.ItemType == ItemTypeRoot {
			return
		}
		rows = append(rows, *entry)
	})
	slices.SortFunc(rows, func(a, b BaselineEntry) int {
		if depth := cmp.Compare(strings.Count(b.Path, "/"), strings.Count(a.Path, "/")); depth != 0 {
			return depth
		}
		return cmp.Compare(a.Path, b.Path)
	})

	return rows
}

// batch forgets a batch of rows, checkpoints the store, then removes their
// entries (releaseBaselineRows).
func (r *releaseRun) batch(ctx context.Context, batch []BaselineEntry) error {
	e := r.engine
	forgotten := make([]*BaselineEntry, 0, len(batch))
	for i := range batch {
		row := &batch[i]
		if releaseRowCrossesSymlink(r.exec, row) {
			// The survey keeps the link and names it; its rows stay.
			continue
		}
		if err := e.baseline.CommitMutation(ctx, releaseForgetMutation(row)); err != nil {
			return e.restoreForgottenRows(ctx, forgotten, fmt.Errorf("sync: forgetting %s before its release: %w", row.Path, err))
		}
		forgotten = append(forgotten, row)
	}
	if err := e.baseline.Checkpoint(ctx); err != nil {
		return e.restoreForgottenRows(ctx, forgotten, fmt.Errorf("sync: releasing the local tree: %w", err))
	}

	for i, row := range forgotten {
		if err := ctx.Err(); err != nil {
			return e.restoreForgottenRows(ctx, forgotten[i:], fmt.Errorf("sync: releasing the local tree: %w", err))
		}
		action := releaseAction(row, r.localByPath[row.Path])
		outcome := r.exec.ExecuteLocalDelete(ctx, &action)
		if outcome.Success && !outcome.KeptLocally {
			r.released++
			continue
		}
		if !e.releaseEntryRemains(row.Path) {
			// Already gone: nothing to remove, and the row stays forgotten.
			continue
		}
		// Refused (the content changed, the trash did not take the file, the
		// folder is not empty) or kept as a folder of ignored content: the
		// entry stays, and so does its row.
		e.logger.Debug("release kept a synced entry", slog.String("path", row.Path), slog.Any("error", outcome.Error))
		var changed *localContentChangedError
		refusal := releaseRefusal{path: row.Path, err: outcome.Error, contentChanged: errors.As(outcome.Error, &changed)}
		r.refused = append(r.refused, refusal)
		restore := []*BaselineEntry{releaseRowToRestore(row, refusal.contentChanged)}
		if r.stopAtFirstRefusal {
			// The rest of the batch comes back in the same restore, so one
			// row that fails to come back does not keep the others out.
			r.stopped = true
			restore = append(restore, forgotten[i+1:]...)
		}
		if err := e.restoreForgottenRows(ctx, restore, nil); err != nil {
			return err
		}
		if r.stopped {
			return nil
		}
	}

	return nil
}

// releaseRowToRestore is the row a refused entry gets back: as it was, or
// without its change time when the delete proved the file's content changed,
// so the next scan reads the file instead of trusting the row.
func releaseRowToRestore(row *BaselineEntry, contentChanged bool) *BaselineEntry {
	if !contentChanged {
		return row
	}
	restored := *row
	restored.LocalCtime = 0

	return &restored
}

// reportReleaseRefusals says why the release could not remove the synced
// entries the survey filed as notRemoved (addRefused): the trash took
// nothing, the file is immutable, a folder would not go. Every later drain
// meets the same refusal, so it is one Warn per streak with the first cause
// and how many entries, repeats at Debug, and Info once a release removes
// what it meets again (R-6.6.30); the report and status name every entry.
func (e *Engine) reportReleaseRefusals(notRemoved []releaseRefusal) {
	var cause error
	var attrs []any
	if len(notRemoved) > 0 {
		first := notRemoved[0]
		cause = fmt.Errorf("%s: %w", first.path, cmp.Or(first.err, errReleaseKeptWithoutCause))
		attrs = append(attrs, slog.Int("entries", len(notRemoved)))
	}
	e.reportStreakOutcome(streakReport{
		failing: &e.releaseRefusalFailing,
		warn: "Could not move a synced entry of a removed shared folder to the trash or delete it; " +
			"the folder stays until it can be removed",
		repeat:    "synced entries of a removed shared folder still cannot be removed",
		recovered: "synced entries of a removed shared folder can be removed again",
	}, cause, attrs...)
}

// releaseEntryRemains reports whether the entry stored at storedPath is on
// disk, read without following a final link. An entry that cannot be read
// counts as gone.
func (e *Engine) releaseEntryRemains(storedPath string) bool {
	_, err := e.syncTree.Lstat(e.syncTree.Spelling(storedPath))

	return err == nil
}

// releaseRowCrossesSymlink reports whether a row's path is, or lies below, a
// symbolic link. The release never acts on one (surveyReleaseTree). An
// unreadable path counts as crossing one: its row stays.
func releaseRowCrossesSymlink(exec *executor, row *BaselineEntry) bool {
	_, crosses, err := exec.symlinkBoundaryForPath(exec.syncTree.Spelling(row.Path))

	return crosses || err != nil
}

// releaseAction is the local delete of one synced entry, planned from its
// baseline row and, for a folder, the local truth the drain's last scan saw.
func releaseAction(row *BaselineEntry, local *localState) Action {
	return Action{
		Type:    ActionLocalDelete,
		DriveID: row.DriveID,
		ItemID:  row.ItemID,
		Path:    row.Path,
		View: &pathView{
			Path:     row.Path,
			Baseline: row,
			Local:    local,
		},
	}
}

// releaseForgetMutation is the commit a local delete of row makes, made
// before the entry is removed (releaseBaselineRows). It is exempt from the
// delete-safety hold: the release is not the user's delete and deletes
// nothing on OneDrive.
func releaseForgetMutation(row *BaselineEntry) *BaselineMutation {
	return &BaselineMutation{
		Action:               ActionLocalDelete,
		Success:              true,
		Path:                 row.Path,
		DriveID:              row.DriveID,
		ItemID:               row.ItemID,
		ItemType:             row.ItemType,
		ExemptFromDeleteHold: true,
	}
}

// releaseRestoreMutation puts a forgotten row back as it was.
func releaseRestoreMutation(row *BaselineEntry) *BaselineMutation {
	return &BaselineMutation{
		Action:                ActionBaselineUpdate,
		Success:               true,
		Path:                  row.Path,
		DriveID:               row.DriveID,
		ItemID:                row.ItemID,
		ParentID:              row.ParentID,
		ItemType:              row.ItemType,
		LocalHash:             row.LocalHash,
		RemoteHash:            row.RemoteHash,
		LocalSize:             row.LocalSize,
		LocalSizeKnown:        row.LocalSizeKnown,
		RemoteSize:            row.RemoteSize,
		RemoteSizeKnown:       row.RemoteSizeKnown,
		LocalMtime:            row.LocalMtime,
		RemoteMtime:           row.RemoteMtime,
		LocalCtime:            row.LocalCtime,
		LocalDevice:           row.LocalDevice,
		LocalInode:            row.LocalInode,
		LocalBirthTime:        row.LocalBirthTime,
		LocalHasIdentity:      row.LocalHasIdentity,
		LocalIdentityObserved: true,
		ETag:                  row.ETag,
	}
}

// restoreForgottenRows puts back the rows whose entries are still on disk and
// returns cause joined with any restore that failed. The restores run on a
// context without the caller's cancellation, bounded like an outcome commit:
// a row whose entry stays must not be lost to a shutdown. A row whose entry
// is gone, or cannot be checked, stays forgotten: an entry without its row is
// adopted by the next run, a row without its entry is not safe.
func (e *Engine) restoreForgottenRows(ctx context.Context, rows []*BaselineEntry, cause error) error {
	restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), OutcomeCommitTimeout)
	defer cancel()

	errs := []error{cause}
	for _, row := range rows {
		if !e.releaseEntryRemains(row.Path) {
			continue
		}
		if err := e.baseline.CommitMutation(restoreCtx, releaseRestoreMutation(row)); err != nil {
			errs = append(errs, fmt.Errorf("sync: restoring the baseline row of %s: %w", row.Path, err))
		}
	}

	return errors.Join(errs...)
}

// emptyReleasedRoot removes the bundled junk left directly in the sync folder
// after a release, under the rule a folder delete applies to junk, and
// returns what still stands that no survey names: an entry the release could
// not remove, which keeps the folder like any other.
func (e *Engine) emptyReleasedRoot(bl *Baseline) (releaseSurvey, error) {
	entries, err := e.syncTree.ReadDir("")
	if err != nil {
		return releaseSurvey{}, fmt.Errorf("sync: reading the sync folder after releasing it: %w", err)
	}
	exec := newExecution(e.execCfg, bl)
	var junk []os.DirEntry
	var survey releaseSurvey
	for _, entry := range entries {
		name := entry.Name()
		if exec.isDisposable(name, entry.IsDir()) && (!entry.IsDir() || exec.findNonDisposable(name) == "") {
			junk = append(junk, entry)
			continue
		}
		survey.notOnOneDrive = append(survey.notOnOneDrive, nfcNormalize(name))
	}
	err = exec.removeDisposableChildren("", junk)
	// Every drain of a folder whose junk stays fails the same way: one Warn
	// per streak, repeats at Debug (R-6.6.30); status names the junk.
	e.reportStreakOutcome(streakReport{
		failing: &e.releasedRootJunkFailing,
		warn: "Could not remove junk from the local folder of a removed shared folder; " +
			"the folder stays until it can be removed",
		repeat:    "junk in the local folder of a removed shared folder still cannot be removed",
		recovered: "junk no longer keeps the local folder of a removed shared folder",
	}, err)
	if err != nil {
		for _, entry := range junk {
			survey.notOnOneDrive = append(survey.notOnOneDrive, nfcNormalize(entry.Name()))
		}
	}

	return survey, nil
}

// recordReleaseKept adds what the release kept to the report and records it
// as removed_folder_kept issues, replacing what an earlier release recorded.
// Content sync does not sync, a folder that cannot be written to and an entry
// the local delete could not remove are kept either way. A new or changed
// file is work for the next drain while the shared folder can be read, and
// kept, as not uploaded, when it cannot. A path that already holds an issue
// of another type keeps that issue, which already names it, and the sync
// folder itself holds none: the report names it.
func (e *Engine) recordReleaseKept(
	ctx context.Context,
	report *Report,
	survey releaseSurvey,
	issues []ObservationIssueRow,
	sharedFolderUnreachable bool,
) error {
	kept := slices.Concat(survey.notOnOneDrive, survey.notWritable, survey.notRemoved)
	report.KeptNotOnOneDrive = newKeptEntries(survey.notOnOneDrive)
	report.KeptNotWritable = newKeptEntries(survey.notWritable)
	report.KeptNotRemoved = newKeptEntries(survey.notRemoved)
	if sharedFolderUnreachable {
		report.KeptNotUploaded = newKeptEntries(survey.unsynced)
		kept = append(kept, survey.unsynced...)
	} else {
		report.LeftForNextSync += len(survey.unsynced)
	}

	otherIssue := make(map[string]bool, len(issues))
	for i := range issues {
		if issues[i].IssueType != issueRemovedFolderKept {
			otherIssue[issues[i].Path] = true
		}
	}
	batch := ObservationFindingsBatch{ManagedIssueTypes: []IssueType{issueRemovedFolderKept}}
	for _, keptPath := range kept {
		if keptPath == "" || otherIssue[keptPath] {
			continue
		}
		batch.Issues = append(batch.Issues, ObservationIssue{
			Path:      keptPath,
			DriveID:   e.driveID,
			IssueType: issueRemovedFolderKept,
		})
	}
	if err := e.baseline.ReconcileObservationFindings(ctx, &batch); err != nil {
		return fmt.Errorf("sync: recording what the release of the local tree kept: %w", err)
	}

	return nil
}
