package sync

import (
	"context"
	"log/slog"
	stdsync "sync"
	"time"
)

// retrierSafetyInterval is the maximum time between retry sweeps.
// Acts as a safety net in case kick signals are lost.
const retrierSafetyInterval = 2 * time.Minute

// InFlightChecker reports whether a path has an in-flight action in the tracker.
type InFlightChecker interface {
	HasInFlight(path string) bool
}

// EventAdder adds a synthetic change event to the buffer.
type EventAdder interface {
	Add(ev *ChangeEvent)
}

// FailureRetrier periodically checks sync_failures for failed items whose
// backoff has expired and re-injects them into the sync pipeline via
// buffer → planner → tracker. This is the sole retry mechanism for sync
// actions (R-6.8.10). The planner re-evaluates each re-injected item
// against current baseline state (conflict detection, filter checks,
// permission checks) — this is the safety gate (failure-redesign.md §12.4).
type FailureRetrier struct {
	state   StateReader
	buf     EventAdder
	tracker InFlightChecker
	logger  *slog.Logger
	nowFunc func() time.Time

	kickCh chan struct{} // 1-buffered
	timer  *time.Timer
	mu     stdsync.Mutex

	// dispatchedRetryAt tracks the last-dispatched next_retry_at for each path.
	// Prevents double-dispatch when bootstrap reconcile and kick signals race:
	// if a row's NextRetryAt matches the tracked value, it was already injected
	// into the buffer. When RecordFailure sets a new next_retry_at (re-failure),
	// the mismatch naturally allows re-dispatch.
	dispatchedRetryAt map[string]int64
}

// NewFailureRetrier creates a FailureRetrier. It does not start until
// Run() is called.
func NewFailureRetrier(
	state StateReader,
	buf EventAdder,
	tracker InFlightChecker,
	logger *slog.Logger,
) *FailureRetrier {
	return &FailureRetrier{
		state:             state,
		buf:               buf,
		tracker:           tracker,
		logger:            logger,
		nowFunc:           time.Now,
		kickCh:            make(chan struct{}, 1),
		dispatchedRetryAt: make(map[string]int64),
	}
}

// Kick sends a non-blocking signal to the reconciler to run a sweep.
// Coalesces multiple kicks — only one sweep runs at a time.
func (r *FailureRetrier) Kick() {
	select {
	case r.kickCh <- struct{}{}:
	default:
		// Already pending — coalesce.
	}
}

// Run is the reconciler's main loop. It performs an initial reconcile sweep,
// then selects on kick signals, safety ticker, and context cancellation.
// Blocks until ctx is canceled.
func (r *FailureRetrier) Run(ctx context.Context) {
	r.logger.Info("failure retrier started")

	// Bootstrap: reconcile immediately to pick up any items reset by
	// crash recovery (ResetInProgressStates).
	r.reconcile(ctx)

	safety := time.NewTicker(retrierSafetyInterval)
	defer safety.Stop()

	for {
		select {
		case <-r.kickCh:
			r.reconcile(ctx)
		case <-safety.C:
			r.reconcile(ctx)
		case <-r.timerChan():
			r.reconcile(ctx)
		case <-ctx.Done():
			r.mu.Lock()
			if r.timer != nil {
				r.timer.Stop()
			}
			r.mu.Unlock()

			r.logger.Info("failure retrier stopped")

			return
		}
	}
}

// timerChan returns the timer's channel, or a nil channel if no timer is set.
// A nil channel in a select blocks forever, which is the desired behavior
// when there are no future retries to wake up for.
func (r *FailureRetrier) timerChan() <-chan time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.timer == nil {
		return nil
	}

	return r.timer.C
}

// reconcile scans sync_failures for retriable failed items and synthesizes
// events for them. The planner re-evaluates each against current baseline.
func (r *FailureRetrier) reconcile(ctx context.Context) {
	now := r.nowFunc()

	r.reconcileSyncFailures(ctx, now)
	r.armTimer(ctx, now)
}

// reconcileSyncFailures handles all failure types in a single sweep over the
// unified sync_failures table.
func (r *FailureRetrier) reconcileSyncFailures(ctx context.Context, now time.Time) {
	rows, err := r.state.ListSyncFailuresForRetry(ctx, now)
	if err != nil {
		r.logger.Warn("failure retrier: failed to list retriable items",
			slog.String("error", err.Error()),
		)

		return
	}

	if len(rows) == 0 {
		return
	}

	dispatched := 0

	for i := range rows {
		row := &rows[i]

		// Skip items already being processed.
		if r.tracker.HasInFlight(row.Path) {
			delete(r.dispatchedRetryAt, row.Path) // pipeline consumed it
			continue
		}

		// Prevent double-dispatch: if this exact row (same next_retry_at)
		// was already injected into the buffer, skip it. This guards
		// against bootstrap reconcile + kick signal racing on the same row.
		// When RecordFailure sets a new next_retry_at (re-failure), the
		// mismatch allows the updated row to be dispatched.
		if lastRetryAt, ok := r.dispatchedRetryAt[row.Path]; ok && lastRetryAt == row.NextRetryAt {
			continue
		}

		// Synthesize event based on direction and inject into the buffer.
		ev := r.synthesizeFailureEvent(row)
		r.buf.Add(ev)
		r.dispatchedRetryAt[row.Path] = row.NextRetryAt
		dispatched++
	}

	if dispatched > 0 {
		r.logger.Info("failure retrier sweep",
			slog.Int("dispatched", dispatched),
		)
	}
}

// synthesizeFailureEvent creates a ChangeEvent from a sync_failures row.
// Upload failures become SourceLocal ChangeModify events; download failures
// become SourceRemote ChangeModify; delete failures become SourceRemote
// ChangeDelete. ItemType defaults to file — the executor looks up the actual
// type during dispatch.
func (r *FailureRetrier) synthesizeFailureEvent(row *SyncFailureRow) *ChangeEvent {
	switch row.Direction {
	case strUpload:
		return &ChangeEvent{
			Source: SourceLocal,
			Type:   ChangeModify,
			Path:   row.Path,
		}
	case strDelete:
		return &ChangeEvent{
			Source:    SourceRemote,
			Type:      ChangeDelete,
			Path:      row.Path,
			ItemID:    row.ItemID,
			DriveID:   row.DriveID,
			ItemType:  ItemTypeFile,
			IsDeleted: true,
		}
	default: // "download"
		return &ChangeEvent{
			Source:   SourceRemote,
			Type:     ChangeModify,
			Path:     row.Path,
			ItemID:   row.ItemID,
			DriveID:  row.DriveID,
			ItemType: ItemTypeFile,
		}
	}
}

// armTimer sets up a timer to fire at the earliest future retry time in the
// sync_failures table. Stops any existing timer first.
func (r *FailureRetrier) armTimer(ctx context.Context, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}

	earliest, err := r.state.EarliestSyncFailureRetryAt(ctx, now)
	if err != nil {
		r.logger.Warn("failure retrier: failed to query earliest retry",
			slog.String("error", err.Error()),
		)

		return
	}

	if earliest.IsZero() {
		return
	}

	delay := earliest.Sub(now)
	if delay <= 0 {
		// Already due — kick immediately.
		r.Kick()
		return
	}

	r.timer = time.AfterFunc(delay, func() {
		r.Kick()
	})
}
