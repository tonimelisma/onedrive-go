package perf

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/panicguard"
)

// Session is one command's performance session: its collector, the periodic
// updates, and the summary Complete writes.
type Session struct {
	name      string
	kind      string
	collector *Collector

	// mu guards logger, which SetLogger replaces while the updates read it.
	mu     sync.Mutex
	logger *slog.Logger
	// doneCh is closed when the periodic updates have ended, or at once for a
	// session without them.
	doneCh chan struct{}
	once   sync.Once
	// cancel ends the context NewSession returned, with a cause.
	cancel context.CancelCauseFunc
}

// NewSession starts a performance session for one command and returns the
// command's context, which carries the session's collector. With an interval
// it logs an update at every interval until that context ends: at Complete,
// or earlier when the caller's context ends, so a command that never reaches
// Complete leaves no updates running. A panic in those updates ends the
// command: it cancels the returned context with the panic as the cause, which
// the command returns and exits 1 with (R-6.3.10), and which the work it
// interrupts reads as a stop (panicguard.AsCancelCause).
func NewSession(
	ctx context.Context,
	logger *slog.Logger,
	kind string,
	name string,
	interval time.Duration,
	parent *Collector,
) (*Session, context.Context) {
	if logger == nil {
		logger = slog.Default()
	}

	ctx, cancel := context.WithCancelCause(ctx)
	collector := NewCollector(parent)
	session := &Session{
		name:      name,
		kind:      kind,
		collector: collector,
		logger:    logger,
		doneCh:    make(chan struct{}),
		cancel:    cancel,
	}

	if interval > 0 {
		go session.runPeriodic(ctx, interval)
	} else {
		close(session.doneCh)
	}

	return session, WithCollector(ctx, collector)
}

// Collector is the session's collector, nil for a nil session.
func (s *Session) Collector() *Collector {
	if s == nil {
		return nil
	}

	return s.collector
}

// SetLogger makes logger the one the updates and the summary go to, once the
// command's own logger exists. A nil logger is ignored.
func (s *Session) SetLogger(logger *slog.Logger) {
	if s == nil || logger == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.logger = logger
}

// Complete ends the session once: it records err's result, releases the
// command's context, which ends the periodic updates, waits for them to end,
// and writes the summary. A later call does nothing.
func (s *Session) Complete(err error) {
	if s == nil {
		return
	}

	s.once.Do(func() {
		if s.collector != nil {
			s.collector.SetResult(resultForError(err))
		}
		// The command is over; release its context. A cause set earlier (a
		// panic in the updates) stays: the first cancel wins.
		s.cancel(nil)
		<-s.doneCh
		s.log("performance summary")
	})
}

// runPeriodic logs an update at every interval until ctx, the command's
// context, ends.
func (s *Session) runPeriodic(ctx context.Context, interval time.Duration) {
	defer close(s.doneCh)
	defer func() {
		if r := recover(); r != nil {
			s.cancel(panicguard.AsCancelCause(panicguard.Recovered(s.currentLogger(), "in the performance logger", r)))
		}
	}()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.log("performance update")
		case <-ctx.Done():
			return
		}
	}
}

func (s *Session) log(message string) {
	if s == nil || s.collector == nil {
		return
	}

	snapshot := s.collector.Snapshot()
	logger := s.currentLogger()
	attrs := append([]slog.Attr{
		slog.String("perf_kind", s.kind),
		slog.String("perf_name", s.name),
	}, SnapshotAttrs(&snapshot)...)
	logger.Info(message, attrsToAny(attrs)...)
}

func (s *Session) currentLogger() *slog.Logger {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.logger == nil {
		return slog.Default()
	}

	return s.logger
}

// resultForError is the summary's result for the command's final error. A
// recovered panic is an error, also when it answers to context.Canceled as
// the cause of the cancel it made.
func resultForError(err error) string {
	switch {
	case err == nil:
		return "success"
	case !errors.Is(err, panicguard.ErrPanic) &&
		(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)):
		return "canceled"
	default:
		return "error"
	}
}

// SnapshotAttrs are the log attributes of snapshot: its duration and result,
// and each counter above zero.
func SnapshotAttrs(snapshot *Snapshot) []slog.Attr {
	if snapshot == nil {
		return nil
	}

	attrs := []slog.Attr{
		slog.Int64("duration_ms", snapshot.DurationMS),
	}

	if snapshot.Result != "" {
		attrs = append(attrs, slog.String("result", snapshot.Result))
	}

	appendCoreSnapshotAttrs(&attrs, snapshot)
	appendSyncSnapshotAttrs(&attrs, snapshot)
	appendStaleWorkSnapshotAttrs(&attrs, snapshot)

	return attrs
}

func appendCoreSnapshotAttrs(attrs *[]slog.Attr, snapshot *Snapshot) {
	appendIntAttr(attrs, "command_items", snapshot.CommandItems)
	appendInt64Attr(attrs, "command_bytes", snapshot.CommandBytes)
	appendIntAttr(attrs, "http_requests", snapshot.HTTPRequestCount)
	appendIntAttr(attrs, "http_successes", snapshot.HTTPSuccessCount)
	appendIntAttr(attrs, "http_client_errors", snapshot.HTTPClientErrorCount)
	appendIntAttr(attrs, "http_server_errors", snapshot.HTTPServerErrorCount)
	appendIntAttr(attrs, "http_transport_errors", snapshot.HTTPTransportErrors)
	appendIntAttr(attrs, "http_retries", snapshot.HTTPRetryCount)
	appendInt64Attr(attrs, "http_time_ms", snapshot.HTTPRequestTimeMS)
	appendInt64Attr(attrs, "http_retry_backoff_ms", snapshot.HTTPRetryBackoffMS)
	appendIntAttr(attrs, "db_transactions", snapshot.DBTransactionCount)
	appendInt64Attr(attrs, "db_transaction_time_ms", snapshot.DBTransactionTimeMS)
	appendIntAttr(attrs, "downloads", snapshot.DownloadCount)
	appendInt64Attr(attrs, "download_bytes", snapshot.DownloadBytes)
	appendIntAttr(attrs, "uploads", snapshot.UploadCount)
	appendInt64Attr(attrs, "upload_bytes", snapshot.UploadBytes)
	appendInt64Attr(attrs, "transfer_time_ms", snapshot.TransferTimeMS)
}

func appendSyncSnapshotAttrs(attrs *[]slog.Attr, snapshot *Snapshot) {
	appendIntAttr(attrs, "observe_runs", snapshot.ObserveRunCount)
	appendIntAttr(attrs, "observed_paths", snapshot.ObservedPathCount)
	appendInt64Attr(attrs, "observe_time_ms", snapshot.ObserveTimeMS)
	appendIntAttr(attrs, "plan_runs", snapshot.PlanRunCount)
	appendIntAttr(attrs, "actionable_actions", snapshot.ActionableActionCount)
	appendInt64Attr(attrs, "plan_time_ms", snapshot.PlanTimeMS)
	appendIntAttr(attrs, "execute_runs", snapshot.ExecuteRunCount)
	appendIntAttr(attrs, "execute_actions", snapshot.ExecuteActionCount)
	appendIntAttr(attrs, "execute_succeeded", snapshot.ExecuteSucceededCount)
	appendIntAttr(attrs, "execute_failed", snapshot.ExecuteFailedCount)
	appendInt64Attr(attrs, "execute_time_ms", snapshot.ExecuteTimeMS)
	appendIntAttr(attrs, "refresh_runs", snapshot.RefreshRunCount)
	appendIntAttr(attrs, "refresh_events", snapshot.RefreshEventCount)
	appendInt64Attr(attrs, "refresh_time_ms", snapshot.RefreshTimeMS)
	appendIntAttr(attrs, "watch_batches", snapshot.WatchBatchCount)
	appendIntAttr(attrs, "watch_paths", snapshot.WatchPathCount)
}

func appendStaleWorkSnapshotAttrs(attrs *[]slog.Attr, snapshot *Snapshot) {
	appendIntAttr(attrs, "superseded_engine_admission", snapshot.SupersededEngineAdmissionCount)
	appendIntAttr(attrs, "superseded_worker_start_local_truth", snapshot.SupersededWorkerStartLocalTruthCount)
	appendIntAttr(attrs, "superseded_worker_start_remote_truth", snapshot.SupersededWorkerStartRemoteTruthCount)
	appendIntAttr(attrs, "superseded_live_local_precondition", snapshot.SupersededLiveLocalPreconditionCount)
	appendIntAttr(attrs, "superseded_live_remote_precondition", snapshot.SupersededLiveRemotePreconditionCount)
	appendIntAttr(attrs, "superseded_pending_replan_retirement", snapshot.SupersededPendingReplanRetirementCount)
	appendIntAttr(attrs, "local_obs_scoped_commits", snapshot.LocalObservationScopedCommitCount)
	appendIntAttr(attrs, "local_obs_scoped_upserts", snapshot.LocalObservationScopedUpsertCount)
	appendIntAttr(attrs, "local_obs_prefix_deletes", snapshot.LocalObservationPrefixDeleteCount)
	appendIntAttr(attrs, "local_obs_full_snapshot_replacements", snapshot.LocalObservationFullSnapshotReplacementCount)
	appendIntAttr(attrs, "local_obs_suspect_dropped_events", snapshot.LocalObservationSuspectDroppedEventsCount)
	appendIntAttr(attrs, "local_obs_suspect_watcher_errors", snapshot.LocalObservationSuspectWatcherErrorCount)
	appendIntAttr(attrs, "local_obs_suspect_full_scan_failed", snapshot.LocalObservationSuspectFullScanFailedCount)
	appendIntAttr(attrs, "local_obs_suspect_other", snapshot.LocalObservationSuspectOtherCount)
	appendInt64Attr(attrs, "replan_idle_waiting_drain_ms", snapshot.ReplanIdleWaitingDrainMS)
	appendInt64Attr(attrs, "replan_idle_local_refresh_ms", snapshot.ReplanIdleLocalRefreshMS)
	appendInt64Attr(attrs, "replan_idle_planning_ms", snapshot.ReplanIdlePlanningMS)
	appendInt64Attr(attrs, "replan_idle_runtime_install_ms", snapshot.ReplanIdleRuntimeInstallMS)
}

func appendIntAttr(attrs *[]slog.Attr, key string, value int) {
	if value > 0 {
		*attrs = append(*attrs, slog.Int(key, value))
	}
}

func appendInt64Attr(attrs *[]slog.Attr, key string, value int64) {
	if value > 0 {
		*attrs = append(*attrs, slog.Int64(key, value))
	}
}

func attrsToAny(attrs []slog.Attr) []any {
	out := make([]any, 0, len(attrs))
	for i := range attrs {
		out = append(out, attrs[i])
	}

	return out
}
