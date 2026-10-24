package cli

import (
	"encoding/json"

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
	Error   string `json:"error,omitempty"`

	// Summary fields.
	DurationMS *int64         `json:"duration_ms,omitempty"`
	Counts     map[string]int `json:"counts,omitempty"`
	Failed     *int           `json:"failed,omitempty"`
	WillRetry  *int           `json:"will_retry,omitempty"`
}

const (
	syncJSONStatusDone   = "done"
	syncJSONStatusFailed = "failed"
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

func (c *syncConsole) emitJSON(event *syncJSONEvent) {
	line, err := json.Marshal(event)
	if err != nil {
		return
	}

	c.write(c.out, string(line))
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
	case a.WillRetry:
		event.Status = "will_retry"
		event.Error = plainErrorReason(a.Err)
	default:
		event.Status = syncJSONStatusFailed
		event.Error = plainErrorReason(a.Err)
	}

	c.emitJSON(event)
}

func (c *syncConsole) jsonSummary(result multisync.RunOnceResult) {
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
		tally := c.tallyFor(report.Identity.MountID, r)
		durationMS := r.Duration.Milliseconds()
		failed := max(tally.failed, r.Failed-tally.willRetry)
		willRetry := tally.willRetry
		event.Status = syncJSONStatusDone
		event.DryRun = r.DryRun
		event.DurationMS = &durationMS
		event.Failed = &failed
		event.WillRetry = &willRetry
		event.Counts = make(map[string]int, len(tally.counts))
		for kind, n := range tally.counts {
			if n > 0 {
				event.Counts[activityJSONAction(kind)] = n
			}
		}
		c.emitJSON(event)
	}
}
