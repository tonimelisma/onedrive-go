package cli

import (
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// syncJSONEvent is one line of `sync --json` output. Sync streams, so --json
// makes stdout JSON lines: one object per start, file action, summary and
// stop, each tagged by "event". Warnings and the final error stay on stderr.
type syncJSONEvent struct {
	Event   string `json:"event"`
	Drive   string `json:"drive,omitempty"`
	SyncDir string `json:"sync_dir,omitempty"`
	Account string `json:"account,omitempty"`
	Mode    string `json:"mode,omitempty"`
	DryRun  bool   `json:"dry_run,omitempty"`
	Paused  bool   `json:"paused,omitempty"`

	// Activity fields.
	Action  string `json:"action,omitempty"`
	Path    string `json:"path,omitempty"`
	OldPath string `json:"old_path,omitempty"`
	Size    *int64 `json:"size,omitempty"`
	Status  string `json:"status,omitempty"`
	// Condition is, for an activity a condition only the user can clear
	// refused, that condition as `status --json` names it in its `type`.
	Condition string `json:"condition,omitempty"`
	Error     string `json:"error,omitempty"`

	// Summary fields.
	DurationMS *int64         `json:"duration_ms,omitempty"`
	Counts     map[string]int `json:"counts,omitempty"`
	Failed     *int           `json:"failed,omitempty"`
	WillRetry  *int           `json:"will_retry,omitempty"`
	StillOpen  *int           `json:"still_open,omitempty"`
}

const (
	syncJSONStatusDone   = "done"
	syncJSONStatusFailed = "failed"
	// syncJSONStatusIncomplete is a drive's summary when its run finished
	// but left work (Report.LeftWork), and the run exits 1 (R-6.6.28).
	syncJSONStatusIncomplete = "incomplete"
	// syncJSONStatusStillOpen is an activity a condition only the user can
	// clear refused: the file is counted in the summary's still_open.
	syncJSONStatusStillOpen = "still_open"
)

func activityJSONAction(kind syncengine.ActivityKind) string {
	switch kind {
	case syncengine.ActivityDownload, syncengine.ActivityUpload:
		return activityVerb(kind)
	case syncengine.ActivityLocalDelete:
		return "delete_local"
	case syncengine.ActivityRemoteDelete:
		return "delete_remote"
	case syncengine.ActivityLocalMove:
		return "move_local"
	case syncengine.ActivityRemoteMove:
		return "move_remote"
	case syncengine.ActivityFolderCreate:
		return "create_folder"
	case syncengine.ActivityConflictCopy:
		return "conflict_copy"
	}

	return "other"
}

func syncModeJSON(mode syncengine.SyncMode) string {
	switch mode {
	case syncengine.SyncDownloadOnly:
		return "download_only"
	case syncengine.SyncUploadOnly:
		return "upload_only"
	case syncengine.SyncBidirectional:
		return "bidirectional"
	}

	return ""
}

// emitJSON writes event as one line of stdout. A line that cannot be written
// does not stop the sync: the failure is said once on stderr, and the lines
// after it are still attempted.
func (c *syncConsole) emitJSON(event *syncJSONEvent) {
	if err := c.writeJSONEvent(event); err != nil {
		c.printErr("", "Warning: sync --json output is incomplete: "+err.Error())
	}
}

// writeJSONEvent writes event under mu, so lines from several mounts never
// interleave. It returns only the first failure and drops the rest on
// purpose: one warning says the stream is incomplete, and a stdout that is
// gone (a closed pipe) must not repeat it for every later line.
func (c *syncConsole) writeJSONEvent(event *syncJSONEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	err := writeJSONLine(c.out, event)
	if err == nil || c.jsonFailureReported {
		return nil
	}
	c.jsonFailureReported = true

	return err
}

func (c *syncConsole) jsonMountEvents(event string, mounts []multisync.StandaloneMountConfig, mode syncengine.SyncMode, dryRun bool) {
	for i := range mounts {
		m := &mounts[i]
		c.emitJSON(&syncJSONEvent{
			Event:   event,
			Drive:   m.DisplayName,
			SyncDir: m.SyncRoot,
			Account: m.AccountEmail,
			Mode:    syncModeJSON(mode),
			DryRun:  dryRun,
			Paused:  m.Paused,
		})
	}
}

//nolint:gocritic // Matches the orchestrator hook signature (see Activity).
func (c *syncConsole) jsonActivity(ma multisync.MountActivity) {
	a := &ma.Activity
	event := &syncJSONEvent{
		Event:   "activity",
		Drive:   ma.DisplayName,
		Action:  activityJSONAction(a.Kind),
		Path:    a.Path,
		OldPath: a.OldPath,
	}
	if a.SizeKnown {
		size := a.Size
		event.Size = &size
	}

	switch {
	case a.Planned:
		event.Status = "planned"
	case a.Success:
		event.Status = syncJSONStatusDone
	case a.ConditionKey != "":
		event.Status = syncJSONStatusStillOpen
		event.Condition = string(a.ConditionType)
		event.Error = plainErrorReason(a.Err)
	case a.WillRetry:
		event.Status = "will_retry"
		event.Error = plainErrorReason(a.Err)
	default:
		event.Status = syncJSONStatusFailed
		event.Error = plainErrorReason(a.Err)
	}

	c.emitJSON(event)
}

// jsonSummary writes one summary line per drive report. Its counts are the
// text summary's (left, from Report.LeftWork): a drive that left work is
// "incomplete", and will_retry includes the work left for the next sync.
func (c *syncConsole) jsonSummary(result multisync.RunOnceResult, left map[string]syncengine.LeftWork) {
	for _, report := range result.Reports {
		if report == nil {
			continue
		}

		event := &syncJSONEvent{Event: "summary", Drive: report.DisplayName}
		if report.Err != nil {
			event.Status = syncJSONStatusFailed
			event.Error = formatMountReportErrorMessage(report)
			c.emitJSON(event)
			continue
		}
		if report.Report == nil {
			continue
		}

		r := report.Report
		mountID := report.Identity.MountID
		tally := c.tallyFor(mountID, r)
		counts := left[mountID]
		durationMS := r.Duration.Milliseconds()
		event.Status = syncJSONStatusDone
		if counts.Any() {
			event.Status = syncJSONStatusIncomplete
		}
		event.DryRun = r.DryRun
		event.DurationMS = &durationMS
		notSynced := counts.NotSynced()
		event.Failed = &counts.Failed
		event.WillRetry = &counts.WillRetry
		event.StillOpen = &notSynced
		event.Counts = make(map[string]int, len(tally.counts))
		for kind, n := range tally.counts {
			if n > 0 {
				event.Counts[activityJSONAction(kind)] = n
			}
		}
		c.emitJSON(event)
	}
}
