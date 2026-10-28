package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// Constants for the remote observer (satisfy mnd linter).
const (
	maxDeltaPaginationGuard = 10000
	// cursorServerErrorResetAfter is how long one delta token may keep
	// failing with 5xx in watch mode before it is treated as unusable and
	// the drive is re-enumerated (R-2.15.2). Graph outages are shorter; a
	// token stuck on a server fault never recovers on its own.
	cursorServerErrorResetAfter = 6 * time.Hour
	// expiredDeltaRecoveryMinInterval spaces whole-drive recoveries in watch
	// mode. A recovery that fails the same way each time (its own
	// enumeration loops, every fresh token is refused) would otherwise
	// re-enumerate the drive back to back.
	expiredDeltaRecoveryMinInterval = 15 * time.Minute
	maxPathDepth                    = 256
	MinPollInterval                 = 30 * time.Second
	specialFolderVault              = "vault" // Graph API personal vault folder name
)

type watchStepResult struct {
	stop bool
	err  error
	woke bool
}

// remoteWatchBatchHandler lets the engine translate a raw remote poll into one
// loop-owned remote observation batch. The observer does not commit durable
// state itself; it blocks until the watch loop applies the batch.
type remoteWatchBatchHandler func(
	ctx context.Context,
	events []changeEvent,
	newToken string,
	topology shortcutTopologyBatch,
) (remoteObservationBatch, error)

// remoteObserver transforms Graph API delta responses into []ChangeEvent.
// It handles pagination, path materialization, change classification, and
// normalization (NFC, driveID zero-padding). Delegates item conversion to
// an embedded ItemConverter configured for primary drive observation.
type remoteObserver struct {
	fetcher             deltaFetcher
	Converter           *itemConverter
	logger              *slog.Logger
	driveID             driveid.ID
	shortcutNamespaceID string
	protectedRoots      []ProtectedRoot
	clock               syncClock

	// mu protects deltaToken for concurrent reads via CurrentDeltaToken().
	// The watch loop is the only writer; helper calls may read concurrently.
	mu         stdsync.Mutex
	deltaToken string

	// lastActivityNano tracks the most recent successful poll time as Unix
	// nanoseconds. Used by the engine to detect stalled observers (B-125).
	lastActivityNano atomic.Int64

	// stats tracks observer-level metrics (B-127).
	stats observerCounters

	// recoverExpiredDelta, when set, replaces Watch's bare re-enumeration
	// after a 410 with the engine's full refresh, the only path that also
	// finds the deletes the expired window lost (R-2.15.3). It returns the
	// batch to emit; the batch's cursor token becomes the watch token.
	recoverExpiredDelta deltaExpiredRecovery
}

// deltaExpiredRecovery runs a full refresh for an expired delta token.
type deltaExpiredRecovery func(ctx context.Context, kind resyncKind) (remoteObservationBatch, error)

// SetDeltaExpiredRecovery wires the full refresh Watch runs after a 410.
func (o *remoteObserver) SetDeltaExpiredRecovery(recovery deltaExpiredRecovery) {
	o.recoverExpiredDelta = recovery
}

// observerCounters holds atomic counters for observer metrics (B-127).
type observerCounters struct {
	eventsEmitted  atomic.Int64
	pollsCompleted atomic.Int64
	errors         atomic.Int64
	hashesComputed atomic.Int64 // items with non-empty content hash (B-282)
}

// observerStats is a snapshot of observer metrics returned by Stats() (B-127).
type observerStats struct {
	EventsEmitted  int64
	PollsCompleted int64
	Errors         int64
	HashesComputed int64 // items processed with a content hash (B-282)
}

// newRemoteObserver creates a RemoteObserver for the given drive. The
// baseline must be a loaded Baseline (from SyncStore.Load); it is
// read-only during observation. The caller must pass a normalized driveid.ID.
func newRemoteObserver(
	fetcher deltaFetcher, baseline *Baseline, driveID driveid.ID, logger *slog.Logger,
) *remoteObserver {
	obs := &remoteObserver{
		fetcher: fetcher,
		logger:  logger,
		driveID: driveID,
		clock:   realClock{},
	}
	obs.Converter = newPrimaryConverter(baseline, driveID, logger, &obs.stats, nil)

	return obs
}

func (o *remoteObserver) SetItemClient(items itemClient) {
	if o == nil || o.Converter == nil {
		return
	}

	o.Converter.Items = items
}

func (o *remoteObserver) SetShortcutTopology(
	namespaceID string,
	protectedRoots []ProtectedRoot,
) {
	if o == nil {
		return
	}

	o.shortcutNamespaceID = namespaceID
	o.protectedRoots = append([]ProtectedRoot(nil), protectedRoots...)
	if o.Converter != nil {
		o.Converter.ProtectedRootBindings = protectedRootByBinding(protectedRoots)
	}
}

// FullDelta fetches all delta pages and returns the accumulated change events
// plus the new delta token (DeltaLink URL) for the next sync pass.
func (o *remoteObserver) FullDelta(ctx context.Context, savedToken string) ([]changeEvent, string, error) {
	events, token, _, err := o.FullDeltaWithShortcutTopology(ctx, savedToken)
	return events, token, err
}

func (o *remoteObserver) FullDeltaWithShortcutTopology(
	ctx context.Context,
	savedToken string,
) ([]changeEvent, string, shortcutTopologyBatch, error) {
	events, token, topology, _, err := o.fullDelta(ctx, savedToken, nil)
	return events, token, topology, err
}

// FullEnumerationWithShortcutTopology enumerates the whole drive and also
// returns the ID of every live item the enumeration returned, whether or not
// it converted into an event. Absence from a complete enumeration is the
// evidence a full refresh deletes on, so it must be absence from what Graph
// returned, not from what the converter kept: an item it skipped (an orphan
// whose parent it could not fetch, for one) was still there (D-008).
func (o *remoteObserver) FullEnumerationWithShortcutTopology(
	ctx context.Context,
) ([]changeEvent, string, shortcutTopologyBatch, map[string]struct{}, error) {
	enumerated := make(map[string]struct{})
	events, token, topology, _, err := o.fullDelta(ctx, "", enumerated)
	if err != nil {
		return nil, "", shortcutTopologyBatch{}, nil, err
	}
	return events, token, topology, enumerated, nil
}

// fullDelta enumerates delta pages and also reports how many non-root items
// the window carried. That count, not the converted event count, says whether
// the window was empty: deletes of never-synced items convert to no events but
// still mean the change log has moved on.
func (o *remoteObserver) fullDelta(
	ctx context.Context,
	savedToken string,
	enumerated map[string]struct{},
) ([]changeEvent, string, shortcutTopologyBatch, int, error) {
	o.logger.Debug("remote observer starting delta enumeration",
		slog.String("drive_id", o.driveID.String()),
		slog.Bool("has_token", savedToken != ""),
	)

	var events []changeEvent
	windowItems := 0
	topology := shortcutTopologyBatch{
		NamespaceID: o.shortcutNamespaceID,
		Kind:        shortcutTopologyObservationIncremental,
	}
	if savedToken == "" {
		topology.Kind = shortcutTopologyObservationComplete
	}
	inflight := make(map[string]inflightParent)
	token := savedToken
	lastProgressLog := time.Now()
	if o.Converter != nil {
		o.Converter.resetIncompleteObservation()
		o.Converter.ShortcutTopology = &topology
		o.Converter.ProtectedRootBindings = protectedRootByBinding(o.protectedRoots)
		defer func() {
			o.Converter.ShortcutTopology = nil
		}()
	}

	loop := newDeltaLoopGuard(savedToken)
	// The whole window is collected before anything is classified: a child
	// can arrive pages before its parent, and resolving it page by page spent
	// the window's GetItem budget on ancestors the window itself carried
	// (R-2.15.4).
	var window []graph.Item
	for page := range maxDeltaPaginationGuard {
		pageRaw, pageItems, newToken, done, err := o.fetchPageRetryingRejection(ctx, token, page, inflight, enumerated)
		if err != nil {
			o.stats.errors.Add(1)

			return nil, "", shortcutTopologyBatch{}, 0, err
		}

		window = append(window, pageRaw...)
		windowItems += pageItems

		// Periodic progress logging for long-running enumerations (100K+ item
		// drives can take minutes). Time-based (30s) rather than page-based to
		// produce evenly-spaced logs regardless of page size or API latency.
		if !done && time.Since(lastProgressLog) >= 30*time.Second {
			o.logger.Info("remote observer delta enumeration in progress",
				slog.Int("pages_fetched", page+1),
				slog.Int("items_so_far", len(window)),
			)

			lastProgressLog = time.Now()
		}

		if done {
			events = o.classifyWindow(ctx, window, inflight)
			if o.Converter.observationIncomplete() {
				o.stats.errors.Add(1)

				return nil, "", shortcutTopologyBatch{}, 0, errRemoteObservationIncomplete
			}
			o.Converter.forgetCarriedAncestors()
			o.recordActivity()
			o.stats.pollsCompleted.Add(1)
			o.stats.eventsEmitted.Add(int64(len(events)))

			// Info only when the poll found changes: an empty watch poll
			// every few minutes is not progress worth showing at --verbose.
			completedLevel := slog.LevelDebug
			if len(events) > 0 {
				completedLevel = slog.LevelInfo
			}
			o.logger.Log(ctx, completedLevel, "remote observer completed delta enumeration",
				slog.Int("pages", page+1),
				slog.Int("events", len(events)),
			)

			return events, newToken, topology, windowItems, nil
		}

		if loopErr := loop.next(newToken, pageRaw); loopErr != nil {
			o.stats.errors.Add(1)
			o.logger.Warn("OneDrive's change list loops; rescanning the whole drive",
				slog.String("reason", loopErr.reason), slog.Int("pages_fetched", page+1))

			return nil, "", shortcutTopologyBatch{}, 0, loopErr
		}
		token = newToken
	}

	return nil, "", shortcutTopologyBatch{}, 0,
		fmt.Errorf("read OneDrive changes: more than %d pages in one change list", maxDeltaPaginationGuard)
}

// Watch continuously polls for remote delta changes and sends events to the
// provided channel. It blocks until the context is canceled, returning nil.
// The optional batch handler lets the engine own watch-mode commit/scope policy
// without mutating RemoteObserver after construction. On transient errors, it
// applies exponential backoff (starting at 5s, capped at the poll interval).
// On ErrDeltaExpired (410), it runs the engine's full refresh through the
// recovery set with SetDeltaExpiredRecovery (orphan detection included) and
// continues from its token; without one it resets the token and re-enumerates.
// The delta token is tracked internally — use CurrentDeltaToken() to read the
// latest value.
func (o *remoteObserver) Watch(
	ctx context.Context,
	savedToken string,
	batches chan<- remoteObservationBatch,
	interval time.Duration,
	wakeCh <-chan struct{},
	handleBatch remoteWatchBatchHandler,
) error {
	if interval < MinPollInterval {
		o.logger.Warn("poll_interval "+interval.String()+" is below the minimum "+MinPollInterval.String()+
			"; using "+MinPollInterval.String(),
			slog.Duration("requested", interval),
			slog.Duration("minimum", MinPollInterval),
		)

		interval = MinPollInterval
	}

	o.setDeltaToken(savedToken)

	o.logger.Info("remote observer starting watch loop",
		slog.String("drive_id", o.driveID.String()),
		slog.Duration("interval", interval),
		slog.Bool("has_token", savedToken != ""),
	)

	bo := retry.NewBackoff(retry.WatchRemotePolicy())
	bo.SetMaxOverride(interval)
	var outage pollOutageLog
	var stuck cursorServerErrorTracker
	var lastRecovery time.Time

	for {
		drainWakeSignals(wakeCh)

		token := o.CurrentDeltaToken()

		polledEvents, newToken, topology, windowItems, err := o.fullDelta(ctx, token, nil)
		if stuck.observe(o.clock.Now(), token, err) {
			o.logger.Warn("OneDrive has failed every check of the saved change-list position for hours; rescanning the whole drive",
				slog.Duration("failing_for", cursorServerErrorResetAfter),
				slog.String("error", err.Error()))
			err = newCursorUnusableError("OneDrive kept failing the saved position", err)
		}
		if err != nil && errors.Is(err, errDeltaExpired) && o.recoverExpiredDelta != nil {
			result := o.recoverExpiredDeltaInWatch(ctx, batches, resyncKindOf(err), interval, bo, &outage, &lastRecovery, wakeCh)
			if result.err != nil {
				return result.err
			}
			if result.stop {
				return nil
			}

			continue
		}
		if err != nil {
			result := o.handleWatchPollError(ctx, bo, &outage, err, wakeCh)
			if result.err != nil {
				return result.err
			}
			if result.stop {
				return nil
			}

			continue
		}
		outage.recovered(o.logger, "Checking OneDrive for changes works again")

		result := o.handleWatchBatch(
			ctx, batches, polledEvents, windowItems, newToken, topology, interval, bo, wakeCh, handleBatch,
		)
		if result.err != nil {
			return result.err
		}
		if result.stop {
			return nil
		}
	}
}

func (o *remoteObserver) handleWatchBatch(
	ctx context.Context,
	batches chan<- remoteObservationBatch,
	polledEvents []changeEvent,
	windowItems int,
	newToken string,
	topology shortcutTopologyBatch,
	interval time.Duration,
	bo *retry.Backoff,
	wakeCh <-chan struct{},
	handleBatch remoteWatchBatchHandler,
) watchStepResult {
	// Only a window that carried no items is empty. A window of items that all
	// converted to nothing (deletes of never-synced items) must still advance
	// the token, or every poll replays it forever.
	if windowItems == 0 && len(polledEvents) == 0 && !topology.shouldApply() {
		return o.handleZeroEventPoll(ctx, interval, bo, wakeCh)
	}

	batch := remoteObservationBatch{
		emitted: append([]changeEvent(nil), polledEvents...),
	}
	if handleBatch != nil {
		var handleErr error
		batch, handleErr = handleBatch(ctx, polledEvents, newToken, topology)
		if handleErr != nil {
			if watchShouldStop(ctx, handleErr) {
				return watchStepResult{stop: true}
			}
			return watchStepResult{err: handleErr}
		}
	}

	if emitErr := o.emitWatchBatch(ctx, batches, &batch); emitErr != nil {
		if watchShouldStop(ctx, emitErr) {
			return watchStepResult{stop: true}
		}
		return watchStepResult{err: emitErr}
	}

	o.setDeltaToken(newToken)
	bo.Reset()

	return o.sleepWatch(ctx, interval, "interval", wakeCh)
}

// cursorServerErrorTracker measures how long one saved delta token has been
// failing with 5xx without a success in between.
type cursorServerErrorTracker struct {
	since time.Time
}

// observe records one poll result and reports whether the token has now
// failed with 5xx for cursorServerErrorResetAfter. Any other result, or a
// poll without a saved token, restarts the measurement.
func (t *cursorServerErrorTracker) observe(now time.Time, token string, err error) bool {
	if token == "" || err == nil || !errors.Is(err, graph.ErrServerError) {
		t.since = time.Time{}
		return false
	}
	if t.since.IsZero() {
		t.since = now
		return false
	}
	if now.Sub(t.since) < cursorServerErrorResetAfter {
		return false
	}

	t.since = time.Time{}
	return true
}

// recoverExpiredDeltaInWatch runs the engine's full refresh for an expired
// token, emits its batch, and continues from the token it produced. A failed
// refresh keeps the expired token, so the next poll answers 410 again and the
// recovery is retried after the usual backoff.
func (o *remoteObserver) recoverExpiredDeltaInWatch(
	ctx context.Context,
	batches chan<- remoteObservationBatch,
	kind resyncKind,
	interval time.Duration,
	bo *retry.Backoff,
	outage *pollOutageLog,
	lastRecovery *time.Time,
	wakeCh <-chan struct{},
) watchStepResult {
	// Recoveries are spaced: one that fails the same way every time must not
	// re-enumerate the whole drive back to back. Wake signals do not shorten
	// this wait.
	if !lastRecovery.IsZero() {
		if wait := expiredDeltaRecoveryMinInterval - o.clock.Now().Sub(*lastRecovery); wait > 0 {
			if result := o.sleepForWatch(ctx, wait, "recovery spacing"); result.stop || result.err != nil {
				return result
			}
		}
	}
	*lastRecovery = o.clock.Now()

	// Info: self-healing, the full refresh re-enumerates the drive.
	o.logger.Info("OneDrive change history expired; rescanning the whole drive (this may take a while)")

	batch, err := o.recoverExpiredDelta(ctx, kind)
	if err != nil {
		return o.backOffAfterFailedRecovery(ctx, bo, outage, err, wakeCh)
	}
	if emitErr := o.emitWatchBatch(ctx, batches, &batch); emitErr != nil {
		if watchShouldStop(ctx, emitErr) {
			return watchStepResult{stop: true}
		}
		return watchStepResult{err: emitErr}
	}

	o.setDeltaToken(batch.cursorToken)
	bo.Reset()

	return o.sleepWatch(ctx, interval, "interval", wakeCh)
}

// backOffAfterFailedRecovery waits out the backoff after a recovery failed.
// The expired token is kept, so the next poll answers 410 again and the
// recovery is retried; the failure is never treated as a fresh expiry, which
// reset the token and the backoff and spun the loop with no wait at all.
func (o *remoteObserver) backOffAfterFailedRecovery(
	ctx context.Context,
	bo *retry.Backoff,
	outage *pollOutageLog,
	err error,
	wakeCh <-chan struct{},
) watchStepResult {
	if watchShouldStop(ctx, err) {
		return watchStepResult{stop: true}
	}

	delay := bo.Next()
	outage.failed(o.logger, "Can't rescan OneDrive after its change history expired; retrying",
		fmt.Errorf("recover expired OneDrive change history: %w", err), delay,
		slog.Int("consecutive_errors", bo.Consecutive()),
	)

	result := o.sleepWatch(ctx, delay, "recovery backoff", wakeCh)
	if result.stop || result.err != nil {
		return result
	}

	return watchStepResult{}
}

func (o *remoteObserver) handleWatchPollError(
	ctx context.Context,
	bo *retry.Backoff,
	outage *pollOutageLog,
	err error,
	wakeCh <-chan struct{},
) watchStepResult {
	if watchShouldStop(ctx, err) {
		return watchStepResult{stop: true}
	}

	if errors.Is(err, errDeltaExpired) {
		// Self-healing: the next poll re-enumerates from scratch.
		o.logger.Info("OneDrive change history expired; rescanning the whole drive (this may take a while)")
		o.setDeltaToken("")
		bo.Reset()

		return watchStepResult{}
	}

	delay := bo.Next()
	outage.failed(o.logger, "Can't check OneDrive for changes; retrying", err, delay,
		slog.Int("consecutive_errors", bo.Consecutive()),
	)

	result := o.sleepWatch(ctx, delay, "backoff", wakeCh)
	if result.stop || result.err != nil {
		return result
	}
	if result.woke {
		bo.Reset()
	}

	return watchStepResult{}
}

func (o *remoteObserver) handleZeroEventPoll(
	ctx context.Context,
	interval time.Duration,
	bo *retry.Backoff,
	wakeCh <-chan struct{},
) watchStepResult {
	// Skip token advancement and observation commit when the window carried no
	// items. The old token replays the same empty window at zero cost,
	// but avoids advancing past deletions still propagating through the
	// Graph change log (ci_issues.md §20).
	o.logger.Debug("watch: delta returned 0 events, skipping token advancement")
	bo.Reset()

	return o.sleepWatch(ctx, interval, "zero-event", wakeCh)
}

func (o *remoteObserver) emitWatchBatch(
	ctx context.Context,
	batches chan<- remoteObservationBatch,
	batch *remoteObservationBatch,
) error {
	if batch == nil {
		return nil
	}

	select {
	case batches <- *batch:
	case <-ctx.Done():
		return fmt.Errorf("emit remote watch batch: %w", ctx.Err())
	}

	return batch.waitApplied(ctx)
}

func (o *remoteObserver) sleepWatch(
	ctx context.Context,
	delay time.Duration,
	phase string,
	wakeCh <-chan struct{},
) watchStepResult {
	if wakeCh == nil {
		return o.sleepForWatch(ctx, delay, phase)
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return watchStepResult{stop: true}
	case <-timer.C:
		return watchStepResult{}
	case <-wakeCh:
		o.logger.Debug("remote watch awakened",
			slog.String("phase", phase),
			slog.String("drive_id", o.driveID.String()),
		)
		return watchStepResult{woke: true}
	}
}

func (o *remoteObserver) sleepForWatch(ctx context.Context, delay time.Duration, phase string) watchStepResult {
	if sleepErr := o.clock.Sleep(ctx, delay); sleepErr != nil {
		if watchShouldStop(ctx, sleepErr) {
			return watchStepResult{stop: true}
		}

		return watchStepResult{err: fmt.Errorf("remote watch %s sleep: %w", phase, sleepErr)}
	}

	return watchStepResult{}
}

func drainWakeSignals(wakeCh <-chan struct{}) {
	if wakeCh == nil {
		return
	}

	for {
		select {
		case <-wakeCh:
		default:
			return
		}
	}
}

func watchShouldStop(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	return false
}

// CurrentDeltaToken returns the latest delta token observed by Watch().
// Thread-safe — can be called from any goroutine while Watch() is running.
func (o *remoteObserver) CurrentDeltaToken() string {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.deltaToken
}

// LastActivity returns the time of the most recent successful poll.
// Returns zero time if no poll has completed. Thread-safe — can be called
// from any goroutine while Watch() is running (B-125).
func (o *remoteObserver) LastActivity() time.Time {
	nano := o.lastActivityNano.Load()
	if nano == 0 {
		return time.Time{}
	}

	return time.Unix(0, nano)
}

// recordActivity updates the liveness timestamp to now.
func (o *remoteObserver) recordActivity() {
	o.lastActivityNano.Store(time.Now().UnixNano())
}

// Stats returns a snapshot of observer metrics. Thread-safe (B-127).
func (o *remoteObserver) Stats() observerStats {
	return observerStats{
		EventsEmitted:  o.stats.eventsEmitted.Load(),
		PollsCompleted: o.stats.pollsCompleted.Load(),
		Errors:         o.stats.errors.Load(),
		HashesComputed: o.stats.hashesComputed.Load(),
	}
}

// setDeltaToken updates the internal delta token under the mutex.
func (o *remoteObserver) setDeltaToken(token string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.deltaToken = token
}

// timeSleep waits for the given duration or until the context is canceled.
// Shared by RemoteObserver, LocalObserver, and ExecutorConfig as the default
// sleep implementation. Each injects it via their sleepFunc field for testability.
func timeSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("remote watch sleep: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

// fetchPageRetryingRejection fetches one page and retries it once when
// OneDrive refuses the token with a non-transient 4xx. A token refused twice
// is unusable (R-2.15.2): retrying it every poll never succeeds, so it takes
// the expired-token recovery instead.
func (o *remoteObserver) fetchPageRetryingRejection(
	ctx context.Context, token string, page int, inflight map[string]inflightParent,
	enumerated map[string]struct{},
) ([]graph.Item, int, string, bool, error) {
	events, items, next, done, err := o.fetchPage(ctx, token, page, inflight, enumerated)
	if err == nil || token == "" || !isCursorRejection(err) {
		return events, items, next, done, err
	}

	o.logger.Debug("OneDrive refused the change-list token; retrying it once",
		slog.Int("page", page), slog.String("error", err.Error()))
	events, items, next, done, err = o.fetchPage(ctx, token, page, inflight, enumerated)
	if err == nil || !isCursorRejection(err) {
		return events, items, next, done, err
	}

	o.logger.Warn("OneDrive keeps refusing the saved change-list position; rescanning the whole drive",
		slog.String("error", err.Error()))

	return nil, 0, "", false, newCursorUnusableError("OneDrive refused the saved position twice", err)
}

// isCursorRejection reports whether a delta fetch failed with a 4xx that
// says the request itself, the token, is bad: not sign-in (401), permission
// (403), timeout (408), expiry (410, handled as such), lock (423) or
// throttling (429).
func isCursorRejection(err error) bool {
	if errors.Is(err, errDeltaExpired) {
		return false
	}
	var graphErr *graph.GraphError
	if !errors.As(err, &graphErr) {
		return false
	}
	switch graphErr.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout,
		http.StatusGone, http.StatusLocked, http.StatusTooManyRequests:
		return false
	default:
		return graphErr.StatusCode >= http.StatusBadRequest && graphErr.StatusCode < http.StatusInternalServerError
	}
}

// deltaLoopGuard notices a change list that leads back on itself: a nextLink
// already fetched in this window, or a non-empty page whose item IDs repeat
// an earlier page exactly. Followed, such a list ran to the 10,000-page guard
// with every item buffered in memory (R-2.15.2).
type deltaLoopGuard struct {
	tokens map[string]struct{}
	pages  map[string]struct{}
}

func newDeltaLoopGuard(savedToken string) *deltaLoopGuard {
	guard := &deltaLoopGuard{tokens: map[string]struct{}{}, pages: map[string]struct{}{}}
	if savedToken != "" {
		guard.tokens[savedToken] = struct{}{}
	}

	return guard
}

func (g *deltaLoopGuard) next(nextToken string, pageItems []graph.Item) *deltaExpiredError {
	if _, seen := g.tokens[nextToken]; seen {
		return newCursorUnusableError("a page links back to an earlier page", nil)
	}
	g.tokens[nextToken] = struct{}{}

	ids := make([]string, 0, len(pageItems))
	for i := range pageItems {
		if !pageItems[i].IsRoot {
			ids = append(ids, pageItems[i].ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	slices.Sort(ids)
	fingerprint := strings.Join(ids, "\x00")
	if _, seen := g.pages[fingerprint]; seen {
		return newCursorUnusableError("a page repeats the items of an earlier page", nil)
	}
	g.pages[fingerprint] = struct{}{}

	return nil
}

// fetchPage fetches a single delta page and registers its items in inflight.
// Returns done=true with the DeltaLink when the final page is reached, or
// done=false with NextLink for continued pagination. Classification waits for
// the whole window (classifyWindow).
//
// Registering every item before any is classified (B-281) matters because the
// Graph API does not guarantee parent-before-child ordering, within a page or
// across pages: vault and package descendants are recognized, and paths
// materialized, only once their ancestors are known.
func (o *remoteObserver) fetchPage(
	ctx context.Context, token string, page int, inflight map[string]inflightParent,
	enumerated map[string]struct{},
) (raw []graph.Item, items int, next string, done bool, err error) {
	dp, err := o.fetcher.Delta(ctx, o.driveID, token)
	if err != nil {
		if errors.Is(err, graph.ErrGone) {
			return nil, 0, "", false, newDeltaExpiredError(err)
		}

		return nil, 0, "", false, fmt.Errorf("sync: fetching delta page %d: %w", page, err)
	}

	o.Converter.enrichSparseParentRefs(ctx, dp.Items)

	for i := range dp.Items {
		o.Converter.registerInflight(&dp.Items[i], inflight)
		if !dp.Items[i].IsRoot {
			items++
		}
		if enumerated != nil && !dp.Items[i].IsDeleted && dp.Items[i].ID != "" {
			enumerated[dp.Items[i].ID] = struct{}{}
		}
	}

	if dp.DeltaLink != "" {
		return dp.Items, items, dp.DeltaLink, true, nil
	}

	if dp.NextLink == "" {
		return nil, 0, "", false, fmt.Errorf("sync: delta page %d has neither NextLink nor DeltaLink", page)
	}

	return dp.Items, items, dp.NextLink, false, nil
}

// classifyWindow resolves the ancestors the whole window still lacks, then
// classifies every item. The GetItem budget is spent only on items no page of
// the window and no baseline row can place (R-2.15.4).
func (o *remoteObserver) classifyWindow(
	ctx context.Context, window []graph.Item, inflight map[string]inflightParent,
) []changeEvent {
	o.Converter.resolveMissingAncestors(ctx, window, inflight)

	var events []changeEvent
	for i := range window {
		if ev := o.Converter.ClassifyItem(&window[i], inflight); ev != nil {
			events = append(events, *ev)
		}
	}

	return events
}

// waitText renders a retry delay or interval in human units for console
// messages ("8 s", "5 min", "1 h"), never a raw Go duration.
func waitText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Round(time.Second)/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Round(time.Minute)/time.Minute))
	default:
		return fmt.Sprintf("%d h", int(d.Round(time.Hour)/time.Hour))
	}
}

// pollOutageLog reports a polling loop's failures once per outage instead of
// once per attempt. The first failure after a success is a Warn with the plain
// reason and the retry delay; later attempts in the same outage are Debug; the
// first success after an outage is one Info. A loop owns its pollOutageLog and
// only that loop's goroutine touches it, so it needs no lock.
type pollOutageLog struct {
	failing bool
}

func (l *pollOutageLog) failed(logger *slog.Logger, msg string, err error, delay time.Duration, attrs ...any) {
	if logger == nil {
		return
	}

	fields := append([]any{
		slog.String("error", err.Error()),
		slog.String("retry_in", waitText(delay)),
	}, attrs...)
	if l.failing {
		logger.Debug(msg, fields...)
		return
	}

	l.failing = true
	logger.Warn(msg, fields...)
}

func (l *pollOutageLog) recovered(logger *slog.Logger, msg string) {
	if !l.failing {
		return
	}

	l.failing = false
	if logger != nil {
		logger.Info(msg)
	}
}

// ---------------------------------------------------------------------------
// Pure helper functions
// ---------------------------------------------------------------------------

// classifyItemType determines the ItemType from graph.Item flags.
func classifyItemType(item *graph.Item) ItemType {
	switch {
	case item.IsRoot:
		return ItemTypeRoot
	case item.IsFolder:
		return ItemTypeFolder
	default:
		return ItemTypeFile
	}
}

// toUnixNano converts a time.Time to Unix nanoseconds. Returns 0 for
// the zero time value.
func toUnixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}

	return t.UnixNano()
}

// nfcNormalize applies Unicode NFC normalization to a string. Applied to
// each name segment individually, not to joined paths.
func nfcNormalize(s string) string {
	return norm.NFC.String(s)
}
