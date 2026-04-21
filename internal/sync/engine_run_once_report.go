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
	e.writeSyncStatusBestEffort(ctx, mode, opts.DryRun, &SyncStatusUpdate{
		SyncedAt: e.nowFunc(),
		Duration: report.Duration,
	})

	return report
}

func shouldWriteSyncStatus(mode Mode, dryRun bool) bool {
	return mode == SyncBidirectional && !dryRun
}

func syncStatusFromUpdate(update *SyncStatusUpdate) *SyncStatus {
	if update == nil {
		return nil
	}

	lastError := ""
	if len(update.Errors) > 0 {
		lastError = update.Errors[0].Error()
	}

	syncedAt := update.SyncedAt
	if syncedAt.IsZero() {
		syncedAt = time.Now()
	}

	return &SyncStatus{
		LastSyncedAt:       syncedAt.UnixNano(),
		LastSyncDurationMs: update.Duration.Milliseconds(),
		LastSucceededCount: update.Succeeded,
		LastFailedCount:    update.Failed,
		LastError:          lastError,
	}
}

func (e *Engine) writeSyncStatusBestEffort(
	ctx context.Context,
	mode Mode,
	dryRun bool,
	update *SyncStatusUpdate,
) {
	if !shouldWriteSyncStatus(mode, dryRun) {
		return
	}

	status := syncStatusFromUpdate(update)
	if status == nil {
		return
	}

	if metaErr := e.baseline.WriteSyncStatus(ctx, status); metaErr != nil {
		e.logger.Warn("failed to write sync status", slog.String("error", metaErr.Error()))
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
