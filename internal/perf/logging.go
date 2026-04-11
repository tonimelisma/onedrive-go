package perf

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

type Session struct {
	name      string
	kind      string
	collector *Collector

	mu     sync.Mutex
	logger *slog.Logger
	stopCh chan struct{}
	doneCh chan struct{}
	once   sync.Once
}

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

	collector := NewCollector(parent)
	session := &Session{
		name:      name,
		kind:      kind,
		collector: collector,
		logger:    logger,
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}

	if interval > 0 {
		go session.runPeriodic(interval)
	} else {
		close(session.doneCh)
	}

	return session, WithCollector(ctx, collector)
}

func (s *Session) Collector() *Collector {
	if s == nil {
		return nil
	}

	return s.collector
}

func (s *Session) SetLogger(logger *slog.Logger) {
	if s == nil || logger == nil {
		return
	}

	s.mu.Lock()
	s.logger = logger
	s.mu.Unlock()
}

func (s *Session) Complete(err error) {
	if s == nil {
		return
	}

	s.once.Do(func() {
		if s.collector != nil {
			s.collector.SetResult(resultForError(err))
		}
		close(s.stopCh)
		<-s.doneCh
		s.log("performance summary")
	})
}

func (s *Session) runPeriodic(interval time.Duration) {
	defer close(s.doneCh)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.log("performance update")
		case <-s.stopCh:
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

func resultForError(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	default:
		return "error"
	}
}

func SnapshotAttrs(snapshot *Snapshot) []slog.Attr {
	if snapshot == nil {
		return nil
	}

	attrs := []slog.Attr{
		slog.Int64("duration_ms", snapshot.DurationMS),
	}
	appendIntAttr := func(key string, value int) {
		if value > 0 {
			attrs = append(attrs, slog.Int(key, value))
		}
	}
	appendInt64Attr := func(key string, value int64) {
		if value > 0 {
			attrs = append(attrs, slog.Int64(key, value))
		}
	}

	if snapshot.Result != "" {
		attrs = append(attrs, slog.String("result", snapshot.Result))
	}

	appendIntAttr("command_items", snapshot.CommandItems)
	appendInt64Attr("command_bytes", snapshot.CommandBytes)
	appendIntAttr("http_requests", snapshot.HTTPRequestCount)
	appendIntAttr("http_successes", snapshot.HTTPSuccessCount)
	appendIntAttr("http_client_errors", snapshot.HTTPClientErrorCount)
	appendIntAttr("http_server_errors", snapshot.HTTPServerErrorCount)
	appendIntAttr("http_transport_errors", snapshot.HTTPTransportErrors)
	appendIntAttr("http_retries", snapshot.HTTPRetryCount)
	appendInt64Attr("http_time_ms", snapshot.HTTPRequestTimeMS)
	appendInt64Attr("http_retry_backoff_ms", snapshot.HTTPRetryBackoffMS)
	appendIntAttr("db_transactions", snapshot.DBTransactionCount)
	appendInt64Attr("db_transaction_time_ms", snapshot.DBTransactionTimeMS)
	appendIntAttr("downloads", snapshot.DownloadCount)
	appendInt64Attr("download_bytes", snapshot.DownloadBytes)
	appendIntAttr("uploads", snapshot.UploadCount)
	appendInt64Attr("upload_bytes", snapshot.UploadBytes)
	appendInt64Attr("transfer_time_ms", snapshot.TransferTimeMS)
	appendIntAttr("observe_runs", snapshot.ObserveRunCount)
	appendIntAttr("observed_paths", snapshot.ObservedPathCount)
	appendInt64Attr("observe_time_ms", snapshot.ObserveTimeMS)
	appendIntAttr("plan_runs", snapshot.PlanRunCount)
	appendIntAttr("planned_actions", snapshot.PlannedActionCount)
	appendInt64Attr("plan_time_ms", snapshot.PlanTimeMS)
	appendIntAttr("execute_runs", snapshot.ExecuteRunCount)
	appendIntAttr("execute_actions", snapshot.ExecuteActionCount)
	appendIntAttr("execute_succeeded", snapshot.ExecuteSucceededCount)
	appendIntAttr("execute_failed", snapshot.ExecuteFailedCount)
	appendInt64Attr("execute_time_ms", snapshot.ExecuteTimeMS)
	appendIntAttr("reconcile_runs", snapshot.ReconcileRunCount)
	appendIntAttr("reconcile_events", snapshot.ReconcileEventCount)
	appendInt64Attr("reconcile_time_ms", snapshot.ReconcileTimeMS)
	appendIntAttr("watch_batches", snapshot.WatchBatchCount)
	appendIntAttr("watch_paths", snapshot.WatchPathCount)

	return attrs
}

func attrsToAny(attrs []slog.Attr) []any {
	out := make([]any, 0, len(attrs))
	for i := range attrs {
		out = append(out, attrs[i])
	}

	return out
}
