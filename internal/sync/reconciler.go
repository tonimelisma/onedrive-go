package sync

import (
	"context"
	"log/slog"
	stdsync "sync"
	"time"
)

// FailureRetrier constants.
const (
	// defaultEscalationThreshold is the failure_count at which a row is escalated
	// to a user-visible conflict instead of being retried.
	defaultEscalationThreshold = 10

	// retrierSafetyInterval is the maximum time between retry sweeps.
	// Acts as a safety net in case kick signals are lost.
	retrierSafetyInterval = 2 * time.Minute
)

// FailureRetrierConfig holds tunable thresholds for the failure retrier.
type FailureRetrierConfig struct {
	EscalationThreshold int // failure count before escalation to conflict
}

// DefaultFailureRetrierConfig returns a FailureRetrierConfig with production defaults.
func DefaultFailureRetrierConfig() FailureRetrierConfig {
	return FailureRetrierConfig{
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

// LocalIssueEscalator marks a local issue as permanently failed.
// Used by the failure retrier when transient issues exceed the retry threshold.
type LocalIssueEscalator interface {
	MarkLocalIssuePermanent(ctx context.Context, path string) error
}

// FailureRetrier periodically checks remote_state and local_issues for failed
// items whose backoff has expired and re-injects them into the sync pipeline.
// Remote items that exceed the threshold are escalated to user-visible conflicts;
// local issues that exceed the threshold are marked permanently_failed.
type FailureRetrier struct {
	cfg         FailureRetrierConfig
	state       StateReader
	escalator   ConflictEscalator
	localIssues LocalIssueEscalator
	buf         EventAdder
	tracker     InFlightChecker
	logger      *slog.Logger
	nowFunc     func() time.Time

	kickCh chan struct{} // 1-buffered
	timer  *time.Timer
	mu     stdsync.Mutex
}

// NewFailureRetrier creates a FailureRetrier. It does not start until
// Run() is called. localIssues may be nil if local issue retry is not needed.
func NewFailureRetrier(
	cfg FailureRetrierConfig,
	state StateReader,
	escalator ConflictEscalator,
	localIssues LocalIssueEscalator,
	buf EventAdder,
	tracker InFlightChecker,
	logger *slog.Logger,
) *FailureRetrier {
	return &FailureRetrier{
		cfg:         cfg,
		state:       state,
		escalator:   escalator,
		localIssues: localIssues,
		buf:         buf,
		tracker:     tracker,
		logger:      logger,
		nowFunc:     time.Now,
		kickCh:      make(chan struct{}, 1),
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

// reconcile scans remote_state and local_issues for retriable failed items,
// escalates those exceeding the threshold, and synthesizes events for the rest.
func (r *FailureRetrier) reconcile(ctx context.Context) {
	now := r.nowFunc()

	r.reconcileRemote(ctx, now)
	r.reconcileLocalIssues(ctx, now)
	r.armTimer(ctx, now)
}

// reconcileRemote handles remote_state failed items.
func (r *FailureRetrier) reconcileRemote(ctx context.Context, now time.Time) {
	rows, err := r.state.ListFailedForRetry(ctx, now)
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
				r.logger.Warn("failure retrier: failed to escalate",
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
		r.logger.Info("failure retrier remote sweep",
			slog.Int("dispatched", dispatched),
			slog.Int("escalated", escalated),
		)
	}
}

// reconcileLocalIssues handles local_issues with expired backoff.
// Transient issues exceeding the threshold are marked permanently_failed;
// others get a SourceLocal ChangeModify event injected to trigger re-upload.
func (r *FailureRetrier) reconcileLocalIssues(ctx context.Context, now time.Time) {
	if r.localIssues == nil {
		return
	}

	rows, err := r.state.ListLocalIssuesForRetry(ctx, now)
	if err != nil {
		r.logger.Warn("failure retrier: failed to list local issues for retry",
			slog.String("error", err.Error()),
		)

		return
	}

	if len(rows) == 0 {
		return
	}

	dispatched := 0
	escalated := 0

	for i := range rows {
		row := &rows[i]

		if r.tracker.HasInFlight(row.Path) {
			continue
		}

		if row.FailureCount >= r.cfg.EscalationThreshold {
			if escErr := r.localIssues.MarkLocalIssuePermanent(ctx, row.Path); escErr != nil {
				r.logger.Warn("failure retrier: failed to escalate local issue",
					slog.String("path", row.Path),
					slog.String("error", escErr.Error()),
				)
			} else {
				escalated++
			}

			continue
		}

		r.buf.Add(&ChangeEvent{
			Source: SourceLocal,
			Type:   ChangeModify,
			Path:   row.Path,
		})
		dispatched++
	}

	if dispatched > 0 || escalated > 0 {
		r.logger.Info("failure retrier local sweep",
			slog.Int("dispatched", dispatched),
			slog.Int("escalated", escalated),
		)
	}
}

// synthesizeEvent creates a ChangeEvent from a failed remote_state row.
// delete_failed and pending_delete rows become ChangeDelete events;
// everything else becomes ChangeModify (re-download).
// Returns nil if the row has an invalid item type (corrupt data).
func (r *FailureRetrier) synthesizeEvent(row *RemoteStateRow) *ChangeEvent {
	changeType := ChangeModify

	if row.SyncStatus == statusDeleteFailed || row.SyncStatus == statusPendingDelete {
		changeType = ChangeDelete
	}

	itemType, err := ParseItemType(row.ItemType)
	if err != nil {
		r.logger.Warn("failure retrier: skipping row with invalid item type",
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

// armTimer sets up a timer to fire at the earliest future retry time across
// both remote_state and local_issues. Stops any existing timer first.
func (r *FailureRetrier) armTimer(ctx context.Context, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}

	earliest, err := r.state.EarliestRetryAt(ctx, now)
	if err != nil {
		r.logger.Warn("failure retrier: failed to query earliest remote retry",
			slog.String("error", err.Error()),
		)

		return
	}

	// Also check local issues for earlier retry times.
	if r.localIssues != nil {
		localEarliest, localErr := r.state.EarliestLocalIssueRetryAt(ctx, now)
		if localErr != nil {
			r.logger.Warn("failure retrier: failed to query earliest local issue retry",
				slog.String("error", localErr.Error()),
			)
		} else if !localEarliest.IsZero() && (earliest.IsZero() || localEarliest.Before(earliest)) {
			earliest = localEarliest
		}
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
