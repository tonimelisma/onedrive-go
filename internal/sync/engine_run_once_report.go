package sync

import (
	"context"
	"log/slog"
	"time"
)

func (e *Engine) completeRunOnceWithoutChanges(
	ctx context.Context,
	start time.Time,
	mode Mode,
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
	e.writeOneShotRunStatusBestEffort(ctx, report)

	return report
}

func (e *Engine) writeOneShotRunStatusBestEffort(ctx context.Context, report *Report) {
	if report == nil {
		return
	}

	if metaErr := e.baseline.WriteSyncRunStatus(ctx, &SyncRunReport{
		CompletedAt: e.nowFunc(),
		Duration:    report.Duration,
		Succeeded:   report.Succeeded,
		Failed:      report.Failed,
		Errors:      report.Errors,
	}); metaErr != nil {
		e.logger.Warn("failed to write one-shot sync status", slog.String("error", metaErr.Error()))
	}
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
