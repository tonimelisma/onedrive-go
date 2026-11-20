package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/multisync"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// syncConsole owns everything `sync` tells a person: what it is about to do,
// each file it touched, and how the run ended. Activity and summaries go to
// stdout because for sync they are the command's output; failures and
// warnings go to stderr. --quiet keeps only failures, --verbose adds failures
// that will be retried.
type syncConsole struct {
	out     io.Writer
	errOut  io.Writer
	quiet   bool
	verbose bool
	// json makes stdout JSON lines (sync_console_json.go) instead of text.
	json bool
	// stamp prefixes lines with the time of day. Watch mode does this for
	// terminal scrollback; the journal adds its own timestamps.
	stamp bool
	// journal marks stderr lines with the journald error priority.
	journal bool
	// hooked is set once ActivityHook handed the feed to the orchestrator:
	// from then on a mount with no tally had no activity, and its summary
	// must not fall back to the planner's counts.
	hooked bool
	now    func() time.Time
	home   string

	// mu guards multi, tallies, jsonFailureReported and reportedConditions,
	// which engine completion goroutines update concurrently, and serializes
	// writes so lines from several mounts never interleave.
	mu      sync.Mutex
	multi   bool
	tallies map[string]*activityTally
	// jsonFailureReported is set once a sync --json line could not be
	// written and the warning saying so went to stderr.
	jsonFailureReported bool
	// reportedConditions holds, for each path the console reported refused by
	// a condition only the user can clear, that condition, so the retries that
	// follow are not reported again. An activity of the path that names no
	// condition ends the entry, and one the engine marks as the condition
	// starting is reported whatever the entry holds (conditionReportDue), so
	// the map holds only paths whose last activity was such a refusal.
	reportedConditions map[reportedPath]syncdomain.ConditionKey
}

// reportedPath is one path of one mount.
type reportedPath struct {
	mountID string
	path    string
}

// activityTally is what the activity feed reported done for one mount, by
// kind, for the summary line's "uploaded 3 files". It is display only: what
// the mount left is the report's (Report.LeftWork), which the file events
// printed during the run do not change.
type activityTally struct {
	counts map[syncengine.ActivityKind]int
	bytes  map[syncengine.ActivityKind]int64
}

func newSyncConsole(cc *CLIContext, watch bool) *syncConsole {
	if cc == nil {
		return nil
	}

	journal := isWriterJournal(cc.Status())
	// Without a home directory paths are shown in full, which is still correct.
	home, homeErr := os.UserHomeDir()
	if homeErr != nil {
		home = ""
	}

	return &syncConsole{
		out:     cc.Output(),
		errOut:  cc.Status(),
		quiet:   cc.Flags.Quiet,
		json:    cc.Flags.JSON,
		verbose: cc.Flags.Verbose || cc.Flags.Debug,
		stamp:   watch && !journal,
		journal: journal,
		now:     time.Now,
		home:    home,
		tallies: make(map[string]*activityTally),
	}
}

// setMountCount tells the console whether lines need a drive prefix.
func (c *syncConsole) setMountCount(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.multi = n > 1
}

// Starting announces a one-shot run so a long first sync is never silent.
func (c *syncConsole) Starting(mounts []multisync.StandaloneMountConfig, mode syncdomain.SyncMode, dryRun bool) {
	c.setMountCount(len(mounts))
	if c.json {
		c.jsonMountEvents("start", mounts, mode, dryRun)
		return
	}

	verb := driveStateLabelSyncing
	if dryRun {
		verb = "Checking (dry run)"
	}
	for i := range mounts {
		m := &mounts[i]
		if m.Paused {
			continue
		}
		c.printOut("", fmt.Sprintf("%s %s with %s%s...", verb, c.displayPath(m.SyncRoot), m.AccountEmail, modeSuffix(mode)))
	}
}

// Watching announces watch mode, one line per mount.
func (c *syncConsole) Watching(mounts []multisync.StandaloneMountConfig, mode syncdomain.SyncMode) {
	if c == nil {
		return
	}
	c.setMountCount(len(mounts))
	if c.json {
		c.jsonMountEvents("watching", mounts, mode, false)
		return
	}

	allPaused := len(mounts) > 0
	for i := range mounts {
		m := &mounts[i]
		state := ""
		if m.Paused {
			state = " (paused)"
		} else {
			allPaused = false
		}
		c.printOut("", fmt.Sprintf("Watching %s for %s%s%s.", c.displayPath(m.SyncRoot), m.AccountEmail, modeSuffix(mode), state))
	}
	if allPaused {
		// The watch idles rather than exits (R-3.4.2), so say what it is
		// waiting for.
		c.printOut("", "All drives are paused; waiting until one is resumed ('onedrive-go resume').")
	}
	if !c.journal {
		c.printOut("", "Press Ctrl-C to stop.")
	}
}

// Stopped reports a clean end of watch mode.
func (c *syncConsole) Stopped() {
	if c == nil {
		return
	}
	if c.json {
		c.emitJSON(&syncJSONEvent{Event: "stopped"})
		return
	}
	c.printOut("", "Stopped.")
}

// ActivityHook returns the orchestrator callback, or nil for a nil console so
// callers without one keep the orchestrator's no-hook path.
func (c *syncConsole) ActivityHook() func(multisync.MountActivity) {
	if c == nil {
		return nil
	}
	c.hooked = true

	return c.Activity
}

func newActivityTally() *activityTally {
	return &activityTally{
		counts: make(map[syncengine.ActivityKind]int),
		bytes:  make(map[syncengine.ActivityKind]int64),
	}
}

// Activity prints one finished (or, for a dry run, planned) file action. An
// action a condition only the user can clear refused is said when the
// condition starts, and by each run the first time it sees the path refused,
// in the words `status` uses for it: the retries that follow are the same
// condition, which status keeps listing. It is said again when the engine
// marks the condition as starting again, after the path synced or failed
// another way, or when another condition refuses it (conditionReportDue).
//
//nolint:gocritic // Matches the orchestrator hook signature, which passes by value so a consumer cannot mutate engine-owned activity.
func (c *syncConsole) Activity(ma multisync.MountActivity) {
	a := &ma.Activity
	c.record(ma.Identity.MountID, a)
	if !c.conditionReportDue(ma.Identity.MountID, a) {
		return
	}
	if c.json {
		c.jsonActivity(ma)
		return
	}

	prefix := c.mountPrefix(ma.DisplayName)
	switch {
	case a.Success || a.Planned:
		c.printOut(prefix, activitySuccessLine(a))
	case a.ConditionKey != "":
		words := describeStatusCondition(a.ConditionKey)
		c.printErr(prefix, fmt.Sprintf("Can't %s %s: %s. %s %s",
			activityVerb(a.Kind), a.Path, words.Title, words.Reason, words.Action))
	case a.WillRetry:
		if c.verbose {
			c.printErr(prefix, fmt.Sprintf("Could not %s %s, will retry: %s", activityVerb(a.Kind), a.Path, plainErrorReason(a.Err)))
		}
	default:
		c.printErr(prefix, fmt.Sprintf("Failed to %s %s: %s", activityVerb(a.Kind), a.Path, plainErrorReason(a.Err)))
	}
}

// Summary prints how each mount's one-shot run ended and returns what each
// mount that ran left unsynced, keyed by mount ID: the numbers its summary
// printed (Report.LeftWork), which the exit status is taken from
// (runOnceResultError). Errors for a single selected drive are left to the
// command's returned error so they print once.
func (c *syncConsole) Summary(result multisync.RunOnceResult) map[string]syncengine.LeftWork {
	left := leftWorkByMount(result)
	if c.json {
		c.jsonSummary(result, left)
		return left
	}

	reportsBySelection := make(map[int][]*multisync.MountReport, len(result.Reports))
	for _, report := range result.Reports {
		if report != nil {
			reportsBySelection[report.SelectionIndex] = append(reportsBySelection[report.SelectionIndex], report)
		}
	}
	multi := result.Startup.StartupResultCount() > 1

	for i := range result.Startup.Results {
		startup := &result.Startup.Results[i]
		prefix := ""
		if multi {
			prefix = startup.DisplayName + ": "
		}

		if startup.Status != multisync.MountStartupRunnable {
			if !multi {
				continue
			}
			if startup.Status == multisync.MountStartupPaused {
				c.printOut(prefix, "Skipped (paused).")
				continue
			}
			c.printErr(prefix, "Error: "+formatStartupResultMessage(startup))
			continue
		}

		for _, report := range reportsBySelection[startup.SelectionIndex] {
			switch {
			case report.Err != nil:
				if multi {
					c.printErr(prefix, "Error: "+formatMountReportErrorMessage(report))
				}
			case report.Report != nil:
				mountID := report.Identity.MountID
				c.printOut(prefix, c.reportSummary(c.tallyFor(mountID, report.Report), report.Report, left[mountID]))
				if held := heldBackSummary(report.Report); held != "" {
					c.printOut(prefix, held)
				}
				if held := heldDeletesSummary(report.Report); held != "" {
					// The deletes need the user, so the line goes to stderr.
					c.printErr(prefix, held)
				}
			}
		}
	}

	return left
}

// conditionReportDue records what the activity says of its path on mountID
// and reports whether it is printed. An activity that names no condition is
// printed and ends the path's condition entry. One a condition refused is
// printed when the engine marks the condition as starting with it
// (ConditionStarts), and otherwise unless the path's last activity was a
// refusal by the same condition: the console lives for a whole `sync
// --watch`, so a path that synced and is then refused again is a condition
// that starts again, and so is one whose retry work the plan dropped with no
// activity of the path in between, which only the engine sees.
func (c *syncConsole) conditionReportDue(mountID string, a *syncengine.Activity) bool {
	key := reportedPath{mountID: mountID, path: a.Path}

	c.mu.Lock()
	defer c.mu.Unlock()

	if a.ConditionKey == "" {
		delete(c.reportedConditions, key)
		return true
	}
	if !a.ConditionStarts && c.reportedConditions[key] == a.ConditionKey {
		return false
	}
	if c.reportedConditions == nil {
		c.reportedConditions = make(map[reportedPath]syncdomain.ConditionKey)
	}
	c.reportedConditions[key] = a.ConditionKey

	return true
}

func (c *syncConsole) record(mountID string, a *syncengine.Activity) {
	c.mu.Lock()
	defer c.mu.Unlock()

	tally := c.tallies[mountID]
	if tally == nil {
		tally = newActivityTally()
		c.tallies[mountID] = tally
	}

	if !a.Success && !a.Planned {
		return
	}
	tally.counts[a.Kind]++
	if a.SizeKnown {
		tally.bytes[a.Kind] += a.Size
	}
}

func (c *syncConsole) tallyFor(mountID string, r *syncengine.Report) *activityTally {
	c.mu.Lock()
	tally := c.tallies[mountID]
	c.mu.Unlock()

	if tally != nil && !r.DryRun {
		return tally
	}
	if c.hooked && !r.DryRun {
		// The feed was wired and reported nothing: nothing happened. A pass
		// whose only planned action was held as retry work used to fall
		// through to the planner's counts and say "uploaded 1 file" (fault
		// round of 2026-11-03).
		return newActivityTally()
	}

	// Dry runs, and callers without an activity feed, summarize from the
	// planner's counts instead.
	tally = newActivityTally()
	tally.counts[syncengine.ActivityDownload] = r.Downloads
	tally.counts[syncengine.ActivityUpload] = r.Uploads
	tally.counts[syncengine.ActivityLocalDelete] = r.LocalDeletes
	tally.counts[syncengine.ActivityRemoteDelete] = r.RemoteDeletes
	tally.counts[syncengine.ActivityLocalMove] = r.Moves
	tally.counts[syncengine.ActivityFolderCreate] = r.FolderCreates
	tally.counts[syncengine.ActivityConflictCopy] = r.ConflictCopies

	return tally
}

// leftWorkByMount is what each mount whose run produced a report left
// unsynced (Report.LeftWork), keyed by mount ID. A mount that failed or was
// skipped has no entry: its error is the command's.
func leftWorkByMount(result multisync.RunOnceResult) map[string]syncengine.LeftWork {
	left := make(map[string]syncengine.LeftWork, len(result.Reports))
	for _, report := range result.Reports {
		if report == nil || report.Err != nil || report.Report == nil {
			continue
		}
		left[report.Identity.MountID] = report.Report.LeftWork()
	}

	return left
}

// reportSummary is a mount's summary line: what the run did, from the tally,
// and what it left, from left (Report.LeftWork). A mount that left work is
// never "Up to date", even when this run itself had nothing to do.
func (c *syncConsole) reportSummary(tally *activityTally, r *syncengine.Report, left syncengine.LeftWork) string {
	parts := tally.parts()

	if r.DryRun {
		if len(parts) == 0 {
			return "Dry run: nothing to do."
		}
		return "Dry run, nothing was changed. Would " + strings.Join(dryRunParts(tally), ", ") + "."
	}

	if len(parts) == 0 && !left.Any() {
		return fmt.Sprintf("Up to date (%s).", humanDuration(r.Duration))
	}

	line := "Done in " + humanDuration(r.Duration)
	if len(parts) > 0 {
		line += ": " + strings.Join(parts, ", ")
	}
	if left.Failed > 0 {
		line += fmt.Sprintf("; %d failed", left.Failed)
	}
	if left.WillRetry > 0 {
		line += fmt.Sprintf("; %d will be retried on the next sync", left.WillRetry)
	}
	if notSynced := left.NotSynced(); notSynced > 0 {
		line += fmt.Sprintf("; %d still not synced (see 'onedrive-go status')", notSynced)
	}

	return line + "."
}

func (t *activityTally) parts() []string {
	var parts []string
	add := func(kind syncengine.ActivityKind, verb, noun, suffix string) {
		n := t.counts[kind]
		if n == 0 {
			return
		}
		text := verb + " " + countOf(n, noun)
		if b := t.bytes[kind]; b > 0 {
			text += " (" + formatSize(b) + ")"
		}
		parts = append(parts, text+suffix)
	}

	add(syncengine.ActivityDownload, "downloaded", "file", "")
	add(syncengine.ActivityUpload, "uploaded", "file", "")
	add(syncengine.ActivityLocalDelete, "deleted", "file", " locally")
	add(syncengine.ActivityRemoteDelete, "deleted", "file", " on OneDrive")
	add(syncengine.ActivityLocalMove, "moved", "item", "")
	add(syncengine.ActivityRemoteMove, "moved", "item", " on OneDrive")
	add(syncengine.ActivityFolderCreate, "created", "folder", "")
	add(syncengine.ActivityConflictCopy, "kept both versions of", "conflicting file", "")

	return parts
}

func dryRunParts(t *activityTally) []string {
	parts := t.parts()
	for i, part := range parts {
		for past, present := range map[string]string{
			"downloaded ": "download ", "uploaded ": "upload ", "deleted ": "delete ",
			"moved ": "move ", "created ": "create ", "kept both versions of ": "keep both versions of ",
		} {
			if strings.HasPrefix(part, past) {
				parts[i] = present + strings.TrimPrefix(part, past)
				break
			}
		}
	}

	return parts
}

// heldDeletesSummary explains deletes the delete-safety hold kept for
// confirmation, and the one command that applies them.
func heldDeletesSummary(r *syncengine.Report) string {
	if r.HeldLocalDeletes == 0 && r.HeldRemoteDeletes == 0 {
		return ""
	}

	var parts []string
	if r.HeldRemoteDeletes > 0 {
		parts = append(parts, countOf(r.HeldRemoteDeletes, "delete")+" on OneDrive")
	}
	if r.HeldLocalDeletes > 0 {
		parts = append(parts, countOf(r.HeldLocalDeletes, "delete")+" on this computer")
	}

	return fmt.Sprintf("Held %s for confirmation: more was deleted than looks intended. "+
		"Check the sync folder, then run 'onedrive-go sync --confirm-deletes' to apply them.",
		strings.Join(parts, " and "))
}

// heldBackSummary explains work a one-way mode deliberately skipped.
func heldBackSummary(r *syncengine.Report) string {
	d := r.DeferredByMode
	if d.Total() == 0 {
		return ""
	}

	var parts []string
	add := func(n int, noun, suffix string) {
		if n > 0 {
			parts = append(parts, countOf(n, noun)+suffix)
		}
	}
	add(d.Uploads, "upload", "")
	add(d.Downloads, "download", "")
	add(d.RemoteDeletes, "delete", " on OneDrive")
	add(d.LocalDeletes, "local delete", "")
	add(d.Moves, "move", "")
	add(d.FolderCreates, "folder create", "")

	return fmt.Sprintf("Held back by --%s: %s.", r.Mode.String(), strings.Join(parts, ", "))
}

func activitySuccessLine(a *syncengine.Activity) string {
	verb := activityDoneVerb(a.Kind)
	if a.Planned {
		verb = "Would " + activityVerb(a.Kind)
	}

	switch a.Kind {
	case syncengine.ActivityLocalMove, syncengine.ActivityRemoteMove:
		return fmt.Sprintf("%s %s -> %s", verb, a.OldPath, a.Path)
	case syncengine.ActivityDownload, syncengine.ActivityUpload:
		if a.SizeKnown && !a.Planned {
			return fmt.Sprintf("%s %s (%s)", verb, a.Path, formatSize(a.Size))
		}
	case syncengine.ActivityLocalDelete, syncengine.ActivityRemoteDelete,
		syncengine.ActivityFolderCreate, syncengine.ActivityConflictCopy:
	}

	return verb + " " + a.Path
}

func activityDoneVerb(kind syncengine.ActivityKind) string {
	switch kind {
	case syncengine.ActivityDownload:
		return "Downloaded"
	case syncengine.ActivityUpload:
		return "Uploaded"
	case syncengine.ActivityLocalDelete:
		return "Deleted locally"
	case syncengine.ActivityRemoteDelete:
		return "Deleted on OneDrive"
	case syncengine.ActivityLocalMove:
		return "Moved locally"
	case syncengine.ActivityRemoteMove:
		return "Moved on OneDrive"
	case syncengine.ActivityFolderCreate:
		return "Created folder"
	case syncengine.ActivityConflictCopy:
		return "Conflict, kept both versions of"
	}

	return "Processed"
}

func activityVerb(kind syncengine.ActivityKind) string {
	switch kind {
	case syncengine.ActivityDownload:
		return "download"
	case syncengine.ActivityUpload:
		return "upload"
	case syncengine.ActivityLocalDelete:
		return "delete locally"
	case syncengine.ActivityRemoteDelete:
		return "delete on OneDrive"
	case syncengine.ActivityLocalMove:
		return "move locally"
	case syncengine.ActivityRemoteMove:
		return "move on OneDrive"
	case syncengine.ActivityFolderCreate:
		return "create folder"
	case syncengine.ActivityConflictCopy:
		return "keep both versions of"
	}

	return "process"
}

func modeSuffix(mode syncdomain.SyncMode) string {
	switch mode {
	case syncdomain.SyncDownloadOnly:
		return " (download only)"
	case syncdomain.SyncUploadOnly:
		return " (upload only)"
	case syncdomain.SyncBidirectional:
		return ""
	}

	return ""
}

func (c *syncConsole) mountPrefix(displayName string) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.multi && displayName != "" {
		return displayName + ": "
	}

	return ""
}

func (c *syncConsole) displayPath(p string) string {
	if c.home == "" || p == "" {
		return p
	}

	if p == c.home {
		return "~"
	}

	if rel, err := filepath.Rel(c.home, p); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.Join("~", rel)
	}

	return p
}

func (c *syncConsole) linePrefix() string {
	if c.stamp {
		return "[" + c.now().Format(consoleClockFormat) + "] "
	}

	return ""
}

func (c *syncConsole) printOut(prefix, line string) {
	if c.quiet {
		return
	}

	c.write(c.out, c.linePrefix()+prefix+line)
}

func (c *syncConsole) printErr(prefix, line string) {
	journalPrefix := ""
	if c.journal {
		journalPrefix = fmt.Sprintf("<%d>", journalPriorityErr)
	}

	c.write(c.errOut, journalPrefix+c.linePrefix()+prefix+line)
}

func (c *syncConsole) write(w io.Writer, line string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// A console that cannot be written to (closed pipe) must not stop the
	// sync; the log file and exit code still record the outcome.
	if _, err := fmt.Fprintln(w, line); err != nil {
		return
	}
}

// humanDuration renders an elapsed time the way a person would say it.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.1f s", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%d min %d s", int(d/time.Minute), int((d%time.Minute)/time.Second))
	default:
		return fmt.Sprintf("%d h %d min", int(d/time.Hour), int((d%time.Hour)/time.Minute))
	}
}

// plainErrorReason reduces an error chain to the part a user can act on.
func plainErrorReason(err error) string {
	if err == nil {
		return "unknown error"
	}

	if reason := networkErrorReason(err); reason != "" {
		return reason
	}

	if authMessage := authErrorMessage(err); authMessage != "" {
		return "sign-in required"
	}

	return innermostErrorText(err)
}

// innermostErrorText returns the deepest message in a wrap chain, which is
// where the concrete cause ("permission denied", "no space left on device")
// lives; the outer layers only name the code path. An error that wraps
// several (fmt.Errorf with more than one %w) is followed through the last
// one: the ones before it are marks, such as a request not sent.
func innermostErrorText(err error) string {
	for {
		next := errors.Unwrap(err)
		var several interface{ Unwrap() []error }
		if next == nil && errors.As(err, &several) {
			if wrapped := several.Unwrap(); len(wrapped) > 0 {
				next = wrapped[len(wrapped)-1]
			}
		}
		if next == nil {
			break
		}
		err = next
	}

	return err.Error()
}
