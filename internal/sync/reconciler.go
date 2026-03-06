package sync

import (
	"context"
	"log/slog"
	stdsync "sync"
	"time"
)

// Reconciler constants.
const (
	// defaultEscalationThreshold is the failure_count at which a row is escalated
	// to a user-visible conflict instead of being retried.
	defaultEscalationThreshold = 10

	// reconcilerSafetyInterval is the maximum time between reconcile sweeps.
	// Acts as a safety net in case kick signals are lost.
	reconcilerSafetyInterval = 2 * time.Minute
)

// ReconcilerConfig holds tunable thresholds for the reconciler.
type ReconcilerConfig struct {
	EscalationThreshold int // failure count before escalation to conflict
}

// DefaultReconcilerConfig returns a ReconcilerConfig with production defaults.
func DefaultReconcilerConfig() ReconcilerConfig {
	return ReconcilerConfig{
		EscalationThreshold: defaultEscalationThreshold,
	}
}

// InFlightChecker reports whether a path has an in-flight action in the tracker.
type InFlightChecker interface {
	HasInFlight(path string) bool
}

// EventAdder adds a synthetic change event to the buffer.
type EventAdder interface {
	Add(ev *ChangeEvent)
}

// Reconciler periodically checks remote_state for failed items whose backoff
// has expired and re-injects them into the sync pipeline. Items that have
// failed more than cfg.EscalationThreshold times are escalated to
// user-visible conflicts.
type Reconciler struct {
	cfg       ReconcilerConfig
	state     StateReader
	escalator ConflictEscalator
	buf       EventAdder
	tracker   InFlightChecker
	logger    *slog.Logger
	nowFunc   func() time.Time

	kickCh chan struct{} // 1-buffered
	timer  *time.Timer
	mu     stdsync.Mutex
}

// NewReconciler creates a Reconciler. The reconciler does not start until
// Run() is called.
func NewReconciler(
	cfg ReconcilerConfig,
	state StateReader,
	escalator ConflictEscalator,
	buf EventAdder,
	tracker InFlightChecker,
	logger *slog.Logger,
) *Reconciler {
	return &Reconciler{
		cfg:       cfg,
		state:     state,
		escalator: escalator,
		buf:       buf,
		tracker:   tracker,
		logger:    logger,
		nowFunc:   time.Now,
		kickCh:    make(chan struct{}, 1),
	}
}

// Kick sends a non-blocking signal to the reconciler to run a sweep.
// Coalesces multiple kicks — only one sweep runs at a time.
func (r *Reconciler) Kick() {
	select {
	case r.kickCh <- struct{}{}:
	default:
		// Already pending — coalesce.
	}
}

// Run is the reconciler's main loop. It performs an initial reconcile sweep,
// then selects on kick signals, safety ticker, and context cancellation.
// Blocks until ctx is canceled.
func (r *Reconciler) Run(ctx context.Context) {
	r.logger.Info("reconciler started")

	// Bootstrap: reconcile immediately to pick up any items reset by
	// crash recovery (ResetInProgressStates).
	r.reconcile(ctx)

	safety := time.NewTicker(reconcilerSafetyInterval)
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

			r.logger.Info("reconciler stopped")

			return
		}
	}
}

// timerChan returns the timer's channel, or a nil channel if no timer is set.
// A nil channel in a select blocks forever, which is the desired behavior
// when there are no future retries to wake up for.
func (r *Reconciler) timerChan() <-chan time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.timer == nil {
		return nil
	}

	return r.timer.C
}

// reconcile scans for retriable failed items, escalates those exceeding the
// threshold, and synthesizes events for the rest.
func (r *Reconciler) reconcile(ctx context.Context) {
	now := r.nowFunc()

	rows, err := r.state.ListFailedForRetry(ctx, now)
	if err != nil {
		r.logger.Warn("reconciler: failed to list retriable items",
			slog.String("error", err.Error()),
		)

		return
	}

	if len(rows) == 0 {
		r.armTimer(ctx, now)
		return
	}

	dispatched := 0
	escalated := 0

	for i := range rows {
		row := &rows[i]

		// Skip items already being processed.
		if r.tracker.HasInFlight(row.Path) {
			continue
		}

		// Escalate if failure count exceeds threshold.
		if row.FailureCount >= r.cfg.EscalationThreshold {
			if escErr := r.escalator.EscalateToConflict(ctx, row.DriveID, row.ItemID, row.Path, row.LastError); escErr != nil {
				r.logger.Warn("reconciler: failed to escalate",
					slog.String("path", row.Path),
					slog.String("error", escErr.Error()),
				)
			} else {
				escalated++
			}

			continue
		}

		// Synthesize a change event and inject into the buffer.
		ev := r.synthesizeEvent(row)
		if ev == nil {
			continue
		}

		r.buf.Add(ev)
		dispatched++
	}

	if dispatched > 0 || escalated > 0 {
		r.logger.Info("reconciler sweep",
			slog.Int("dispatched", dispatched),
			slog.Int("escalated", escalated),
		)
	}

	r.armTimer(ctx, now)
}

// synthesizeEvent creates a ChangeEvent from a failed remote_state row.
// delete_failed and pending_delete rows become ChangeDelete events;
// everything else becomes ChangeModify (re-download).
// Returns nil if the row has an invalid item type (corrupt data).
func (r *Reconciler) synthesizeEvent(row *RemoteStateRow) *ChangeEvent {
	changeType := ChangeModify

	if row.SyncStatus == statusDeleteFailed || row.SyncStatus == statusPendingDelete {
		changeType = ChangeDelete
	}

	itemType, err := ParseItemType(row.ItemType)
	if err != nil {
		r.logger.Warn("reconciler: skipping row with invalid item type",
			slog.String("path", row.Path),
			slog.String("item_type", row.ItemType),
		)

		return nil
	}

	return &ChangeEvent{
		Source:    SourceRemote,
		Type:      changeType,
		Path:      row.Path,
		ItemID:    row.ItemID,
		ParentID:  row.ParentID,
		DriveID:   row.DriveID,
		ItemType:  itemType,
		Size:      row.Size,
		Hash:      row.Hash,
		Mtime:     row.Mtime,
		ETag:      row.ETag,
		IsDeleted: changeType == ChangeDelete,
	}
}

// armTimer sets up a timer to fire at the earliest future retry time.
// Stops any existing timer first. The entire method runs under mu
// (via defer Unlock), so the r.timer assignment at the end is protected.
// The AfterFunc callback only calls Kick() which does not access r.timer.
func (r *Reconciler) armTimer(ctx context.Context, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}

	earliest, err := r.state.EarliestRetryAt(ctx, now)
	if err != nil {
		r.logger.Warn("reconciler: failed to query earliest retry",
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
