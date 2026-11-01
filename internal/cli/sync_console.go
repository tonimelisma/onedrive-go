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
	now     func() time.Time
	home    string

	// mu guards multi and tallies, which engine completion goroutines update
	// concurrently, and serializes writes so lines from several mounts never
	// interleave.
	mu      sync.Mutex
	multi   bool
	tallies map[string]*activityTally
}

type activityTally struct {
	counts    map[syncengine.ActivityKind]int
	bytes     map[syncengine.ActivityKind]int64
	failed    int
	willRetry int
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
func (c *syncConsole) Starting(mounts []multisync.StandaloneMountConfig, mode syncengine.SyncMode, dryRun bool) {
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
func (c *syncConsole) Watching(mounts []multisync.StandaloneMountConfig, mode syncengine.SyncMode) {
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

	return c.Activity
}

// Activity prints one finished (or, for a dry run, planned) file action.
//
//nolint:gocritic // Matches the orchestrator hook signature, which passes by value so a consumer cannot mutate engine-owned activity.
func (c *syncConsole) Activity(ma multisync.MountActivity) {
	a := &ma.Activity
	c.record(ma.Identity.MountID, a)
	if c.json {
		c.jsonActivity(ma)
		return
	}

	prefix := c.mountPrefix(ma.DisplayName)
	switch {
	case a.Success || a.Planned:
		c.printOut(prefix, activitySuccessLine(a))
	case a.WillRetry:
		if c.verbose {
			c.printErr(prefix, fmt.Sprintf("Could not %s %s, will retry: %s", activityVerb(a.Kind), a.Path, plainErrorReason(a.Err)))
		}
	default:
		c.printErr(prefix, fmt.Sprintf("Failed to %s %s: %s", activityVerb(a.Kind), a.Path, plainErrorReason(a.Err)))
	}
}

// Summary prints how each mount's one-shot run ended. Errors for a single
// selected drive are left to the command's returned error so they print once.
func (c *syncConsole) Summary(result multisync.RunOnceResult) {
	if c.json {
		c.jsonSummary(result)
		return
	}

	reportsBySelection := make(map[int][]*multisync.MountReport, len(result.Reports))
	for _, report := range result.Reports {
		if report != nil {
			reportsBySelection[report.SelectionIndex] = append(reportsBySelection[report.SelectionIndex], report)
		}
	}
	multi := result.Startup.SelectedCount() > 1

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
				c.printOut(prefix, c.reportSummary(report.Identity.MountID, report.Report))
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
}

func (c *syncConsole) record(mountID string, a *syncengine.Activity) {
	c.mu.Lock()
	defer c.mu.Unlock()

	tally := c.tallies[mountID]
	if tally == nil {
		tally = &activityTally{
			counts: make(map[syncengine.ActivityKind]int),
			bytes:  make(map[syncengine.ActivityKind]int64),
		}
		c.tallies[mountID] = tally
	}

	switch {
	case a.Success || a.Planned:
		tally.counts[a.Kind]++
		if a.SizeKnown {
			tally.bytes[a.Kind] += a.Size
		}
	case a.WillRetry:
		tally.willRetry++
	default:
		tally.failed++
	}
}

func (c *syncConsole) tallyFor(mountID string, r *syncengine.Report) *activityTally {
	c.mu.Lock()
	tally := c.tallies[mountID]
	c.mu.Unlock()

	if tally != nil && !r.DryRun {
		return tally
	}

	// Dry runs, and callers without an activity feed, summarize from the
	// planner's counts instead.
	return &activityTally{
		counts: map[syncengine.ActivityKind]int{
			syncengine.ActivityDownload:     r.Downloads,
			syncengine.ActivityUpload:       r.Uploads,
			syncengine.ActivityLocalDelete:  r.LocalDeletes,
			syncengine.ActivityRemoteDelete: r.RemoteDeletes,
			syncengine.ActivityLocalMove:    r.Moves,
			syncengine.ActivityFolderCreate: r.FolderCreates,
			syncengine.ActivityConflictCopy: r.ConflictCopies,
		},
		bytes:  map[syncengine.ActivityKind]int64{},
		failed: r.Failed,
	}
}

func (c *syncConsole) reportSummary(mountID string, r *syncengine.Report) string {
	tally := c.tallyFor(mountID, r)
	parts := tally.parts()
	failed := max(tally.failed, r.Failed-tally.willRetry)

	if r.DryRun {
		if len(parts) == 0 {
			return "Dry run: nothing to do."
		}
		return "Dry run, nothing was changed. Would " + strings.Join(dryRunParts(tally), ", ") + "."
	}

	if len(parts) == 0 && failed == 0 && tally.willRetry == 0 {
		return fmt.Sprintf("Up to date (%s).", humanDuration(r.Duration))
	}

	line := "Done in " + humanDuration(r.Duration)
	if len(parts) > 0 {
		line += ": " + strings.Join(parts, ", ")
	}
	if failed > 0 {
		line += fmt.Sprintf("; %d failed", failed)
	}
	if tally.willRetry > 0 {
		line += fmt.Sprintf("; %d will be retried on the next sync", tally.willRetry)
	}

	return line + "."
}

func (t *activityTally) parts() []string {
	var parts []string
	add := func(kind syncengine.ActivityKind, singular, plural, suffix string) {
		n := t.counts[kind]
		if n == 0 {
			return
		}
		text := fmt.Sprintf("%s %d %s", singular, n, plural)
		if n == 1 {
			text = fmt.Sprintf("%s 1 %s", singular, strings.TrimSuffix(plural, "s"))
		}
		if b := t.bytes[kind]; b > 0 {
			text += " (" + formatSize(b) + ")"
		}
		parts = append(parts, text+suffix)
	}

	add(syncengine.ActivityDownload, "downloaded", "files", "")
	add(syncengine.ActivityUpload, "uploaded", "files", "")
	add(syncengine.ActivityLocalDelete, "deleted", "files", " locally")
	add(syncengine.ActivityRemoteDelete, "deleted", "files", " on OneDrive")
	add(syncengine.ActivityLocalMove, "moved", "items", "")
	add(syncengine.ActivityRemoteMove, "moved", "items", " on OneDrive")
	add(syncengine.ActivityFolderCreate, "created", "folders", "")
	add(syncengine.ActivityConflictCopy, "kept both versions of", "conflicting files", "")

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
		parts = append(parts, fmt.Sprintf("%d on OneDrive", r.HeldRemoteDeletes))
	}
	if r.HeldLocalDeletes > 0 {
		parts = append(parts, fmt.Sprintf("%d on this computer", r.HeldLocalDeletes))
	}

	return fmt.Sprintf("Held %s deletes for confirmation: more was deleted than looks intended. "+
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
	add := func(n int, what string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, what))
		}
	}
	add(d.Uploads, "uploads")
	add(d.Downloads, "downloads")
	add(d.RemoteDeletes, "deletes on OneDrive")
	add(d.LocalDeletes, "local deletes")
	add(d.Moves, "moves")
	add(d.FolderCreates, "folder creates")

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

func modeSuffix(mode syncengine.SyncMode) string {
	switch mode {
	case syncengine.SyncDownloadOnly:
		return " (download only)"
	case syncengine.SyncUploadOnly:
		return " (upload only)"
	case syncengine.SyncBidirectional:
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
// lives; the outer layers only name the code path.
func innermostErrorText(err error) string {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			break
		}
		err = next
	}

	return err.Error()
}
