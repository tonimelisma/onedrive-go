package perf

import (
	"net/http"
	"sync"
	"time"
)

type TransferKind string

const (
	TransferKindDownload TransferKind = "download"
	TransferKindUpload   TransferKind = "upload"
)

type Snapshot struct {
	StartedAt time.Time `json:"started_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`

	DurationMS int64  `json:"duration_ms"`
	Result     string `json:"result,omitempty"`

	CommandItems int   `json:"command_items,omitempty"`
	CommandBytes int64 `json:"command_bytes,omitempty"`

	HTTPRequestCount      int   `json:"http_request_count,omitempty"`
	HTTPSuccessCount      int   `json:"http_success_count,omitempty"`
	HTTPClientErrorCount  int   `json:"http_client_error_count,omitempty"`
	HTTPServerErrorCount  int   `json:"http_server_error_count,omitempty"`
	HTTPTransportErrors   int   `json:"http_transport_errors,omitempty"`
	HTTPRetryCount        int   `json:"http_retry_count,omitempty"`
	HTTPRequestTimeMS     int64 `json:"http_request_time_ms,omitempty"`
	HTTPRetryBackoffMS    int64 `json:"http_retry_backoff_ms,omitempty"`
	DBTransactionCount    int   `json:"db_transaction_count,omitempty"`
	DBTransactionTimeMS   int64 `json:"db_transaction_time_ms,omitempty"`
	DownloadCount         int   `json:"download_count,omitempty"`
	DownloadBytes         int64 `json:"download_bytes,omitempty"`
	UploadCount           int   `json:"upload_count,omitempty"`
	UploadBytes           int64 `json:"upload_bytes,omitempty"`
	TransferTimeMS        int64 `json:"transfer_time_ms,omitempty"`
	ObserveRunCount       int   `json:"observe_run_count,omitempty"`
	ObservedPathCount     int   `json:"observed_path_count,omitempty"`
	ObserveTimeMS         int64 `json:"observe_time_ms,omitempty"`
	PlanRunCount          int   `json:"plan_run_count,omitempty"`
	ActionableActionCount int   `json:"actionable_action_count,omitempty"`
	PlanTimeMS            int64 `json:"plan_time_ms,omitempty"`
	ExecuteRunCount       int   `json:"execute_run_count,omitempty"`
	ExecuteActionCount    int   `json:"execute_action_count,omitempty"`
	ExecuteSucceededCount int   `json:"execute_succeeded_count,omitempty"`
	ExecuteFailedCount    int   `json:"execute_failed_count,omitempty"`
	ExecuteTimeMS         int64 `json:"execute_time_ms,omitempty"`
	ReconcileRunCount     int   `json:"reconcile_run_count,omitempty"`
	ReconcileEventCount   int   `json:"reconcile_event_count,omitempty"`
	ReconcileTimeMS       int64 `json:"reconcile_time_ms,omitempty"`
	WatchBatchCount       int   `json:"watch_batch_count,omitempty"`
	WatchPathCount        int   `json:"watch_path_count,omitempty"`
}

type Collector struct {
	parent *Collector
	nowFn  func() time.Time

	mu    sync.Mutex
	state Snapshot
}

func NewCollector(parent *Collector) *Collector {
	nowFn := time.Now
	startedAt := nowFn()

	return &Collector{
		parent: parent,
		nowFn:  nowFn,
		state: Snapshot{
			StartedAt: startedAt,
			UpdatedAt: startedAt,
		},
	}
}

func (c *Collector) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	snapshot := c.state
	end := snapshot.UpdatedAt
	if snapshot.Result == "" {
		end = c.nowFn()
	}
	snapshot.DurationMS = durationMS(end.Sub(snapshot.StartedAt))

	return snapshot
}

func (c *Collector) SetResult(result string) {
	c.apply(func(snapshot *Snapshot) {
		snapshot.Result = result
	})
}

func (c *Collector) AddCommandItems(count int) {
	if count == 0 {
		return
	}

	c.apply(func(snapshot *Snapshot) {
		snapshot.CommandItems += count
	})
}

func (c *Collector) AddCommandBytes(bytes int64) {
	if bytes == 0 {
		return
	}

	c.apply(func(snapshot *Snapshot) {
		snapshot.CommandBytes += bytes
	})
}

func (c *Collector) RecordHTTPRequest(statusCode int, duration time.Duration, err error) {
	c.apply(func(snapshot *Snapshot) {
		snapshot.HTTPRequestCount++
		snapshot.HTTPRequestTimeMS += durationMS(duration)
		switch {
		case err != nil:
			snapshot.HTTPTransportErrors++
		case statusCode >= http.StatusOK && statusCode < http.StatusMultipleChoices:
			snapshot.HTTPSuccessCount++
		case statusCode >= http.StatusBadRequest && statusCode < http.StatusInternalServerError:
			snapshot.HTTPClientErrorCount++
		case statusCode >= http.StatusInternalServerError:
			snapshot.HTTPServerErrorCount++
		}
	})
}

func (c *Collector) RecordHTTPRetry(backoff time.Duration) {
	c.apply(func(snapshot *Snapshot) {
		snapshot.HTTPRetryCount++
		snapshot.HTTPRetryBackoffMS += durationMS(backoff)
	})
}

func (c *Collector) RecordTransfer(kind TransferKind, bytes int64, duration time.Duration) {
	c.apply(func(snapshot *Snapshot) {
		snapshot.TransferTimeMS += durationMS(duration)
		switch kind {
		case TransferKindDownload:
			snapshot.DownloadCount++
			snapshot.DownloadBytes += bytes
		case TransferKindUpload:
			snapshot.UploadCount++
			snapshot.UploadBytes += bytes
		}
	})
}

func (c *Collector) RecordDBTransaction(duration time.Duration) {
	c.apply(func(snapshot *Snapshot) {
		snapshot.DBTransactionCount++
		snapshot.DBTransactionTimeMS += durationMS(duration)
	})
}

func (c *Collector) RecordObserve(paths int, duration time.Duration) {
	c.apply(func(snapshot *Snapshot) {
		snapshot.ObserveRunCount++
		snapshot.ObservedPathCount += paths
		snapshot.ObserveTimeMS += durationMS(duration)
	})
}

func (c *Collector) RecordPlan(actions int, duration time.Duration) {
	c.apply(func(snapshot *Snapshot) {
		snapshot.PlanRunCount++
		snapshot.ActionableActionCount += actions
		snapshot.PlanTimeMS += durationMS(duration)
	})
}

func (c *Collector) RecordExecute(actions, succeeded, failed int, duration time.Duration) {
	c.apply(func(snapshot *Snapshot) {
		snapshot.ExecuteRunCount++
		snapshot.ExecuteActionCount += actions
		snapshot.ExecuteSucceededCount += succeeded
		snapshot.ExecuteFailedCount += failed
		snapshot.ExecuteTimeMS += durationMS(duration)
	})
}

func (c *Collector) RecordReconcile(events int, duration time.Duration) {
	c.apply(func(snapshot *Snapshot) {
		snapshot.ReconcileRunCount++
		snapshot.ReconcileEventCount += events
		snapshot.ReconcileTimeMS += durationMS(duration)
	})
}

func (c *Collector) RecordWatchBatch(paths int) {
	c.apply(func(snapshot *Snapshot) {
		snapshot.WatchBatchCount++
		snapshot.WatchPathCount += paths
	})
}

func (c *Collector) apply(update func(*Snapshot)) {
	if c == nil {
		return
	}

	now := c.nowFn()
	c.mu.Lock()
	update(&c.state)
	c.state.UpdatedAt = now
	c.mu.Unlock()

	if c.parent != nil {
		c.parent.apply(update)
	}
}

func durationMS(duration time.Duration) int64 {
	return duration.Milliseconds()
}
