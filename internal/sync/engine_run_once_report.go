package sync

import (
	"log/slog"
	"time"
)

func (e *Engine) completeRunOnceWithoutChanges(
	start time.Time,
	mode SyncMode,
	opts RunOptions,
) *Report {
	e.logger.Info("sync pass complete: no changes detected",
		slog.Duration("duration", e.since(start)),
	)

	report := &Report{
		Mode:     mode,
		DryRun:   opts.DryRun,
		Duration: e.since(start),
	}

	return report
}

func (e *Engine) completeDryRunReport(start time.Time, report *Report) *Report {
	report.Duration = e.since(start)

	e.logger.Info("dry-run complete: no changes applied",
		slog.Duration("duration", report.Duration),
		slog.Int("deferred_folder_creates", report.DeferredByMode.FolderCreates),
		slog.Int("deferred_moves", report.DeferredByMode.Moves),
		slog.Int("deferred_downloads", report.DeferredByMode.Downloads),
		slog.Int("deferred_uploads", report.DeferredByMode.Uploads),
		slog.Int("deferred_local_deletes", report.DeferredByMode.LocalDeletes),
		slog.Int("deferred_remote_deletes", report.DeferredByMode.RemoteDeletes),
	)

	return report
}

func (e *Engine) logRunOnceCompletion(report *Report) {
	if report == nil {
		return
	}

	e.logger.Info("sync pass complete",
		slog.Duration("duration", report.Duration),
		slog.Int("succeeded", report.Succeeded),
		slog.Int("failed", report.Failed),
		slog.Int("deferred_folder_creates", report.DeferredByMode.FolderCreates),
		slog.Int("deferred_moves", report.DeferredByMode.Moves),
		slog.Int("deferred_downloads", report.DeferredByMode.Downloads),
		slog.Int("deferred_uploads", report.DeferredByMode.Uploads),
		slog.Int("deferred_local_deletes", report.DeferredByMode.LocalDeletes),
		slog.Int("deferred_remote_deletes", report.DeferredByMode.RemoteDeletes),
	)
}

// withPass folds one more pass of a one-shot sync into the run's report and
// returns it; a nil run report starts from the pass. Work done adds up across
// passes. What is still held back (by the mode or the delete-safety hold) is
// a state, not work: the last pass reports it, so a deferral every pass sees
// is not counted once per pass. The caller sets Duration from the run's start.
func (r *Report) withPass(pass *Report) *Report {
	if pass == nil {
		return r
	}
	if r == nil {
		run := *pass
		run.Errors = append([]error(nil), pass.Errors...)
		return &run
	}

	r.FolderCreates += pass.FolderCreates
	r.Moves += pass.Moves
	r.Downloads += pass.Downloads
	r.Uploads += pass.Uploads
	r.LocalDeletes += pass.LocalDeletes
	r.RemoteDeletes += pass.RemoteDeletes
	r.ConflictCopies += pass.ConflictCopies
	r.BaselineUpdates += pass.BaselineUpdates
	r.Cleanups += pass.Cleanups
	r.Succeeded += pass.Succeeded
	r.Failed += pass.Failed
	r.Errors = append(r.Errors, pass.Errors...)
	r.DeferredByMode = pass.DeferredByMode
	r.HeldLocalDeletes = pass.HeldLocalDeletes
	r.HeldRemoteDeletes = pass.HeldRemoteDeletes

	return r
}
