package syncobserve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/loglevel"
	"github.com/tonimelisma/onedrive-go/internal/retry"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
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
	events []syncdomain.RemoteItem,
	newToken string,
	topology syncshortcut.ShortcutTopologyBatch,
) (RemoteObservationBatch, error)

// RemoteObserver transforms Graph API delta responses into []RemoteItem.
// It handles pagination, path materialization, change classification, and
// normalization (NFC, driveID zero-padding). Delegates item conversion to
// an embedded ItemConverter configured for primary drive observation.
type RemoteObserver struct {
	fetcher             syncdomain.DeltaFetcher
	Converter           *itemConverter
	logger              *slog.Logger
	driveID             driveid.ID
	shortcutNamespaceID string
	protectedRoots      []syncshortcut.ProtectedRoot
	clock               syncdomain.SyncClock

	// mu protects deltaToken for concurrent reads via CurrentDeltaToken().
	// The watch loop is the only writer; helper calls may read concurrently.
	mu         stdsync.Mutex
	deltaToken string

	// stats tracks observer-level metrics (B-127).
	stats observerCounters

	// recoverExpiredDelta, when set, replaces Watch's bare re-enumeration
	// after a 410 with the engine's full refresh, the only path that also
	// finds the deletes the expired window lost (R-2.15.3). It returns the
	// batch to emit; the batch's cursor token becomes the watch token.
	recoverExpiredDelta DeltaExpiredRecovery

	// debugEvents is the engine's debug event sink, set by the watch runtime
	// before Watch; nil reports nothing.
	debugEvents func(syncdomain.EngineDebugEvent)
}

// DeltaExpiredRecovery is the full refresh the engine hands the observer to
// run for an expired delta token.
type DeltaExpiredRecovery func(ctx context.Context, kind syncdomain.ResyncKind) (RemoteObservationBatch, error)

// SetClock replaces the observer's time capability. Must be called before
// the observer fetches.
func (o *RemoteObserver) SetClock(clock syncdomain.SyncClock) {
	o.clock = clock
}

// SetDebugEvents installs the engine's debug event sink (see debugEvents).
// Must be called before Watch.
func (o *RemoteObserver) SetDebugEvents(emit func(syncdomain.EngineDebugEvent)) {
	o.debugEvents = emit
}

// SetDeltaExpiredRecovery wires the full refresh Watch runs after a 410.
func (o *RemoteObserver) SetDeltaExpiredRecovery(recovery DeltaExpiredRecovery) {
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

// NewRemoteObserver creates a RemoteObserver for the given drive. The
// baseline must be a loaded Baseline (from SyncStore.Load); it is
// read-only during observation. The caller must pass a normalized driveid.ID.
func NewRemoteObserver(
	fetcher syncdomain.DeltaFetcher, baseline syncdomain.BaselineView, driveID driveid.ID, logger *slog.Logger,
) *RemoteObserver {
	obs := &RemoteObserver{
		fetcher: fetcher,
		logger:  logger,
		driveID: driveID,
		clock:   syncdomain.RealClock{},
	}
	obs.Converter = NewPrimaryConverter(baseline, driveID, logger, &obs.stats, nil)

	return obs
}

func (o *RemoteObserver) SetItemClient(items syncdomain.ItemClient) {
	if o == nil || o.Converter == nil {
		return
	}

	o.Converter.Items = items
}

// SetRemotePathLookup lets the converter place a deleted item the baseline
// does not hold by the path its remote_state row stores (R-2.4.17).
func (o *RemoteObserver) SetRemotePathLookup(lookup func(itemID string) (string, bool)) {
	if o == nil || o.Converter == nil {
		return
	}
	o.Converter.RemotePathByID = lookup
}

func (o *RemoteObserver) SetShortcutTopology(
	namespaceID string,
	protectedRoots []syncshortcut.ProtectedRoot,
) {
	if o == nil {
		return
	}

	o.shortcutNamespaceID = namespaceID
	o.protectedRoots = append([]syncshortcut.ProtectedRoot(nil), protectedRoots...)
	if o.Converter != nil {
		o.Converter.ProtectedRootBindings = syncshortcut.ProtectedRootByBinding(protectedRoots)
	}
}

// FullDelta fetches all delta pages and returns the accumulated reported items
// plus the new delta token (DeltaLink URL) for the next sync pass.
func (o *RemoteObserver) FullDelta(ctx context.Context, savedToken string) ([]syncdomain.RemoteItem, string, error) {
	events, token, _, err := o.FullDeltaWithShortcutTopology(ctx, savedToken)
	return events, token, err
}

func (o *RemoteObserver) FullDeltaWithShortcutTopology(
	ctx context.Context,
	savedToken string,
) ([]syncdomain.RemoteItem, string, syncshortcut.ShortcutTopologyBatch, error) {
	events, token, topology, _, err := o.fullDelta(ctx, savedToken, nil)
	return events, token, topology, err
}

// FullEnumerationWithShortcutTopology enumerates the whole drive and also
// returns the ID of every live item the enumeration returned, whether or not
// it converted into an event. Absence from a complete enumeration is the
// evidence a full refresh deletes on, so it must be absence from what Graph
// returned, not from what the converter kept: an item it skipped (an orphan
// whose parent it could not fetch, for one) was still there (D-008).
func (o *RemoteObserver) FullEnumerationWithShortcutTopology(
	ctx context.Context,
) ([]syncdomain.RemoteItem, string, syncshortcut.ShortcutTopologyBatch, map[string]struct{}, error) {
	enumerated := make(map[string]struct{})
	events, token, topology, _, err := o.fullDelta(ctx, "", enumerated)
	if err != nil {
		return nil, "", syncshortcut.ShortcutTopologyBatch{}, nil, err
	}
	return events, token, topology, enumerated, nil
}

// fullDelta enumerates delta pages and also reports how many non-root items
// the window carried. That count, not the converted event count, says whether
// the window was empty: deletes of never-synced items convert to no events but
// still mean the change log has moved on.
func (o *RemoteObserver) fullDelta(
	ctx context.Context,
	savedToken string,
	enumerated map[string]struct{},
) ([]syncdomain.RemoteItem, string, syncshortcut.ShortcutTopologyBatch, int, error) {
	o.logger.Debug("remote observer starting delta enumeration",
		slog.String("drive_id", o.driveID.String()),
		slog.Bool("has_token", savedToken != ""),
	)

	var events []syncdomain.RemoteItem
	windowItems := 0
	topology := syncshortcut.ShortcutTopologyBatch{
		NamespaceID: o.shortcutNamespaceID,
		Kind:        syncshortcut.ShortcutTopologyObservationIncremental,
	}
	if savedToken == "" {
		topology.Kind = syncshortcut.ShortcutTopologyObservationComplete
	}
	inflight := make(map[string]inflightParent)
	token := savedToken
	lastProgressLog := o.clock.Now()
	if o.Converter != nil {
		o.Converter.resetIncompleteObservation()
		o.Converter.ShortcutTopology = &topology
		o.Converter.ProtectedRootBindings = syncshortcut.ProtectedRootByBinding(o.protectedRoots)
		defer func() {
			o.Converter.ShortcutTopology = nil
		}()
	}

	loop := newDeltaLoopGuard()
	// The whole window is collected before anything is classified: a child
	// can arrive pages before its parent, and resolving it page by page spent
	// the window's GetItem budget on ancestors the window itself carried
	// (R-2.15.4).
	var window []graph.Item
	for page := range maxDeltaPaginationGuard {
		fetched, err := o.fetchPageRetryingRejection(ctx, token, page, inflight, enumerated)
		if err != nil {
			o.stats.errors.Add(1)

			return nil, "", syncshortcut.ShortcutTopologyBatch{}, 0, err
		}
		pageRaw, pageItems, newToken, done := fetched.raw, fetched.items, fetched.next, fetched.done
		if page == 0 {
			// The window's own first request, with or without a saved token:
			// a next link back to it is a link back like any other.
			loop.asked(fetched.askedKey(token))
		}

		window = append(window, pageRaw...)
		windowItems += pageItems

		// Periodic progress logging for long-running enumerations (100K+ item
		// drives can take minutes). Time-based (30s) rather than page-based to
		// produce evenly-spaced logs regardless of page size or API latency.
		if !done && o.clock.Now().Sub(lastProgressLog) >= 30*time.Second {
			o.logger.Info("remote observer delta enumeration in progress",
				slog.Int("pages_fetched", page+1),
				slog.Int("items_so_far", len(window)),
			)

			lastProgressLog = o.clock.Now()
		}

		if done {
			events = o.classifyWindow(ctx, window, inflight)
			if o.Converter.observationIncomplete() {
				o.stats.errors.Add(1)

				return nil, "", syncshortcut.ShortcutTopologyBatch{}, 0, syncdomain.ErrRemoteObservationIncomplete
			}
			o.Converter.forgetCarriedAncestors()
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

		if loopErr := loop.next(fetched.nextKey(), pageRaw); loopErr != nil {
			o.stats.errors.Add(1)
			o.logger.Warn("OneDrive's change list loops; rescanning the whole drive",
				slog.String("reason", loopErr.Reason()), slog.Int("pages_fetched", page+1))

			return nil, "", syncshortcut.ShortcutTopologyBatch{}, 0, loopErr
		}
		token = newToken
	}

	return nil, "", syncshortcut.ShortcutTopologyBatch{}, 0,
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
func (o *RemoteObserver) Watch(
	ctx context.Context,
	savedToken string,
	batches chan<- RemoteObservationBatch,
	interval time.Duration,
	wakeCh <-chan struct{},
	handleBatch remoteWatchBatchHandler,
) error {
	if interval < syncdomain.MinPollInterval {
		o.logger.Warn("poll_interval "+interval.String()+" is below the minimum "+syncdomain.MinPollInterval.String()+
			"; using "+syncdomain.MinPollInterval.String(),
			slog.Duration("requested", interval),
			slog.Duration("minimum", syncdomain.MinPollInterval),
		)

		interval = syncdomain.MinPollInterval
	}

	o.setDeltaToken(savedToken)

	o.logger.Info("remote observer starting watch loop",
		slog.String("drive_id", o.driveID.String()),
		slog.Duration("interval", interval),
		slog.Bool("has_token", savedToken != ""),
	)

	bo := retry.NewBackoff(retry.WatchRemotePolicy())
	bo.SetMaxOverride(interval)
	var outage PollOutageLog
	var stuck cursorServerErrorTracker
	var lastRecovery time.Time

	for {
		o.drainWakeSignals(wakeCh)

		token := o.CurrentDeltaToken()

		// Taken before the first page is asked for: a create that ended before
		// this time can be in what the poll sees, and the watch loop judges
		// create intents by it (RemoteObservationBatch.pollStartedAt).
		pollStartedAt := o.clock.Now()
		polledEvents, newToken, topology, windowItems, err := o.fullDelta(ctx, token, nil)
		if stuck.observe(o.clock.Now(), token, err) {
			o.logger.Warn("OneDrive has failed every check of the saved change-list position for hours; rescanning the whole drive",
				slog.Duration("failing_for", cursorServerErrorResetAfter),
				slog.Any("error", err))
			err = syncdomain.NewCursorUnusableError("OneDrive kept failing the saved position", err)
		}
		if err != nil && errors.Is(err, syncdomain.ErrDeltaExpired) && o.recoverExpiredDelta != nil {
			result := o.recoverExpiredDeltaInWatch(ctx, batches, syncdomain.ResyncKindOf(err), interval, bo, &outage, &lastRecovery, wakeCh)
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
		outage.Recovered(ctx, o.logger, "Checking OneDrive for changes works again")

		result := o.handleWatchBatch(
			ctx, batches, pollStartedAt, polledEvents, windowItems, newToken, topology, interval, bo, wakeCh, handleBatch,
		)
		if result.err != nil {
			return result.err
		}
		if result.stop {
			return nil
		}
	}
}

func (o *RemoteObserver) handleWatchBatch(
	ctx context.Context,
	batches chan<- RemoteObservationBatch,
	pollStartedAt time.Time,
	polledEvents []syncdomain.RemoteItem,
	windowItems int,
	newToken string,
	topology syncshortcut.ShortcutTopologyBatch,
	interval time.Duration,
	bo *retry.Backoff,
	wakeCh <-chan struct{},
	handleBatch remoteWatchBatchHandler,
) watchStepResult {
	// Only a window that carried no items is empty. A window of items that all
	// converted to nothing (deletes of never-synced items) must still advance
	// the token, or every poll replays it forever.
	if windowItems == 0 && len(polledEvents) == 0 && !topology.ShouldApply() {
		return o.handleZeroEventPoll(ctx, batches, pollStartedAt, interval, bo, wakeCh)
	}

	batch := RemoteObservationBatch{
		Changes: remoteChangesOf(polledEvents),
	}
	if handleBatch != nil {
		var handleErr error
		batch, handleErr = handleBatch(ctx, polledEvents, newToken, topology)
		if handleErr != nil {
			if syncdomain.RunEnded(ctx, handleErr) {
				return watchStepResult{stop: true}
			}
			return watchStepResult{err: handleErr}
		}
	}
	batch.PollStartedAt = pollStartedAt

	if emitErr := o.emitWatchBatch(ctx, batches, &batch); emitErr != nil {
		if syncdomain.RunEnded(ctx, emitErr) {
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
func (o *RemoteObserver) recoverExpiredDeltaInWatch(
	ctx context.Context,
	batches chan<- RemoteObservationBatch,
	kind syncdomain.ResyncKind,
	interval time.Duration,
	bo *retry.Backoff,
	outage *PollOutageLog,
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
	// The full refresh stands in for this poll: it observed OneDrive from the
	// time it began.
	batch.PollStartedAt = *lastRecovery
	if emitErr := o.emitWatchBatch(ctx, batches, &batch); emitErr != nil {
		if syncdomain.RunEnded(ctx, emitErr) {
			return watchStepResult{stop: true}
		}
		return watchStepResult{err: emitErr}
	}

	o.setDeltaToken(batch.CursorToken)
	bo.Reset()

	return o.sleepWatch(ctx, interval, "interval", wakeCh)
}

// backOffAfterFailedRecovery waits out the backoff after a recovery failed.
// The expired token is kept, so the next poll answers 410 again and the
// recovery is retried; the failure is never treated as a fresh expiry, which
// reset the token and the backoff and spun the loop with no wait at all.
func (o *RemoteObserver) backOffAfterFailedRecovery(
	ctx context.Context,
	bo *retry.Backoff,
	outage *PollOutageLog,
	err error,
	wakeCh <-chan struct{},
) watchStepResult {
	if syncdomain.RunEnded(ctx, err) {
		return watchStepResult{stop: true}
	}

	delay := bo.Next()
	outage.Failed(o.logger, "Can't rescan OneDrive after its change history expired; retrying",
		fmt.Errorf("recover expired OneDrive change history: %w", err), delay,
		slog.Int("consecutive_errors", bo.Consecutive()),
	)

	result := o.sleepWatch(ctx, delay, "recovery backoff", wakeCh)
	if result.stop || result.err != nil {
		return result
	}

	return watchStepResult{}
}

func (o *RemoteObserver) handleWatchPollError(
	ctx context.Context,
	bo *retry.Backoff,
	outage *PollOutageLog,
	err error,
	wakeCh <-chan struct{},
) watchStepResult {
	if syncdomain.RunEnded(ctx, err) {
		return watchStepResult{stop: true}
	}

	if errors.Is(err, syncdomain.ErrDeltaExpired) {
		// Self-healing: the next poll re-enumerates from scratch.
		o.logger.Info("OneDrive change history expired; rescanning the whole drive (this may take a while)")
		o.setDeltaToken("")
		bo.Reset()

		return watchStepResult{}
	}

	delay := bo.Next()
	outage.Failed(o.logger, "Can't check OneDrive for changes; retrying", err, delay,
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

// handleZeroEventPoll skips token advancement and observation commit when
// the window carried no items. The old token replays the same empty window at
// zero cost, but avoids advancing past deletions still propagating through
// the Graph change log (ci_issues.md §20). The poll still hands over when it
// began, in a batch that carries nothing else: no items and no token, so it
// commits nothing and replans nothing, while a create intent whose file is
// gone can be dropped once a poll that began after its attempt ended has
// been seen (watchCreateAttempts).
func (o *RemoteObserver) handleZeroEventPoll(
	ctx context.Context,
	batches chan<- RemoteObservationBatch,
	pollStartedAt time.Time,
	interval time.Duration,
	bo *retry.Backoff,
	wakeCh <-chan struct{},
) watchStepResult {
	o.logger.Debug("watch: delta returned 0 events, skipping token advancement")
	if emitErr := o.emitWatchBatch(ctx, batches, &RemoteObservationBatch{
		Source: RemoteObservationBatchPrimaryWatch, PollStartedAt: pollStartedAt, PollOnly: true,
	}); emitErr != nil {
		if syncdomain.RunEnded(ctx, emitErr) {
			return watchStepResult{stop: true}
		}
		return watchStepResult{err: emitErr}
	}
	bo.Reset()

	return o.sleepWatch(ctx, interval, "zero-event", wakeCh)
}

func (o *RemoteObserver) emitWatchBatch(
	ctx context.Context,
	batches chan<- RemoteObservationBatch,
	batch *RemoteObservationBatch,
) error {
	if batch == nil {
		return nil
	}

	select {
	case batches <- *batch:
	case <-ctx.Done():
		return fmt.Errorf("emit remote watch batch: %w", ctx.Err())
	}

	return batch.WaitApplied(ctx)
}

func (o *RemoteObserver) sleepWatch(
	ctx context.Context,
	delay time.Duration,
	phase string,
	wakeCh <-chan struct{},
) watchStepResult {
	if wakeCh == nil {
		return o.sleepForWatch(ctx, delay, phase)
	}

	// The wait is armed on the run's clock, like the wait without a wake
	// channel: a backoff measured on wall time would not move with the clock
	// that paces everything else the run does.
	due := make(chan struct{})
	timer := o.clock.AfterFunc(delay, func() { close(due) })
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return watchStepResult{stop: true}
	case <-due:
		return watchStepResult{}
	case <-wakeCh:
		o.logger.Debug("remote watch awakened",
			slog.String("phase", phase),
			slog.String("drive_id", o.driveID.String()),
		)
		return watchStepResult{woke: true}
	}
}

func (o *RemoteObserver) sleepForWatch(ctx context.Context, delay time.Duration, phase string) watchStepResult {
	if sleepErr := o.clock.Sleep(ctx, delay); sleepErr != nil {
		if syncdomain.RunEnded(ctx, sleepErr) {
			return watchStepResult{stop: true}
		}

		return watchStepResult{err: fmt.Errorf("remote watch %s sleep: %w", phase, sleepErr)}
	}

	return watchStepResult{}
}

// drainWakeSignals drops the wake signals waiting when a poll begins, which
// that poll covers, and reports how many it dropped to the debug event sink.
func (o *RemoteObserver) drainWakeSignals(wakeCh <-chan struct{}) {
	if wakeCh == nil {
		return
	}

	drained := 0
	for {
		select {
		case <-wakeCh:
			drained++
		default:
			if drained > 0 && o.debugEvents != nil {
				o.debugEvents(syncdomain.EngineDebugEvent{
					Type:     syncdomain.EngineDebugEventRemoteWakesCoalesced,
					Observer: syncdomain.EngineDebugObserverRemote,
					Count:    drained,
				})
			}
			return
		}
	}
}

// CurrentDeltaToken returns the latest delta token observed by Watch().
// Thread-safe — can be called from any goroutine while Watch() is running.
func (o *RemoteObserver) CurrentDeltaToken() string {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.deltaToken
}

// Stats returns a snapshot of observer metrics. Thread-safe (B-127).
func (o *RemoteObserver) Stats() observerStats {
	return observerStats{
		EventsEmitted:  o.stats.eventsEmitted.Load(),
		PollsCompleted: o.stats.pollsCompleted.Load(),
		Errors:         o.stats.errors.Load(),
		HashesComputed: o.stats.hashesComputed.Load(),
	}
}

// setDeltaToken updates the internal delta token under the mutex.
func (o *RemoteObserver) setDeltaToken(token string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.deltaToken = token
}

// fetchPageRetryingRejection fetches one page and retries it once when
// OneDrive refuses the token with a non-transient 4xx. A token refused twice
// is unusable (R-2.15.2): retrying it every poll never succeeds, so it takes
// the expired-token recovery instead.
func (o *RemoteObserver) fetchPageRetryingRejection(
	ctx context.Context, token string, page int, inflight map[string]inflightParent,
	enumerated map[string]struct{},
) (windowPage, error) {
	fetched, err := o.fetchPage(ctx, token, page, inflight, enumerated)
	if err == nil || token == "" || !isCursorRejection(err) {
		return fetched, err
	}

	o.logger.Debug("OneDrive refused the change-list token; retrying it once",
		slog.Int("page", page), slog.Any("error", err))
	fetched, err = o.fetchPage(ctx, token, page, inflight, enumerated)
	if err == nil || !isCursorRejection(err) {
		return fetched, err
	}

	o.logger.Warn("OneDrive keeps refusing the saved change-list position; rescanning the whole drive",
		slog.Any("error", err))

	return windowPage{}, syncdomain.NewCursorUnusableError("OneDrive refused the saved position twice", err)
}

// windowPage is one page of a delta window as fetchPage hands it on: its
// items, how many are not the root, the token that continues the window (the
// next link, or the delta link when done), and the paths the graph client
// resolved the page's own request and its next link to.
type windowPage struct {
	raw      []graph.Item
	items    int
	next     string
	done     bool
	path     string
	nextPath string
}

// askedKey is the loop guard's key of the page's own request: the path the
// graph client asked at, else, from a fetcher that does not say, the token
// it was asked with ("" for none, which keys nothing).
func (p *windowPage) askedKey(token string) string {
	if p.path != "" {
		return p.path
	}

	return token
}

// nextKey is the loop guard's key of the page the next link leads to: the
// path it resolves to, else the link as it was spelled.
func (p *windowPage) nextKey() string {
	if p.nextPath != "" {
		return p.nextPath
	}

	return p.next
}

// isCursorRejection reports whether a delta fetch failed with a 4xx that
// says the request itself, the token, is bad: not sign-in (401), permission
// (403), timeout (408), expiry (410, handled as such), lock (423) or
// throttling (429).
func isCursorRejection(err error) bool {
	if errors.Is(err, syncdomain.ErrDeltaExpired) {
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

// deltaLoopGuard notices a change list that leads back on itself: a next link
// to a page already asked for in this window, the window's first request
// included whether or not it carried a saved token, or a non-empty page whose
// item IDs repeat an earlier page exactly. Pages are keyed by the path the
// graph client resolves each request and link to (graph.DeltaPage.Path and
// NextPath), so two spellings of one link are one page. The check runs
// before the page is asked for again. Followed, such a list ran to the
// 10,000-page guard with every item buffered in memory (R-2.15.2).
type deltaLoopGuard struct {
	askedKeys map[string]struct{}
	pages     map[string]struct{}
}

func newDeltaLoopGuard() *deltaLoopGuard {
	return &deltaLoopGuard{askedKeys: map[string]struct{}{}, pages: map[string]struct{}{}}
}

// asked records the key of a page the window asked for; an empty key, a
// first request a fetcher did not describe, records nothing.
func (g *deltaLoopGuard) asked(key string) {
	if key != "" {
		g.askedKeys[key] = struct{}{}
	}
}

func (g *deltaLoopGuard) next(nextKey string, pageItems []graph.Item) *syncdomain.DeltaExpiredError {
	if _, seen := g.askedKeys[nextKey]; seen {
		return syncdomain.NewCursorUnusableError("a page links back to an earlier page", nil)
	}
	g.askedKeys[nextKey] = struct{}{}

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
		return syncdomain.NewCursorUnusableError("a page repeats the items of an earlier page", nil)
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
func (o *RemoteObserver) fetchPage(
	ctx context.Context, token string, page int, inflight map[string]inflightParent,
	enumerated map[string]struct{},
) (windowPage, error) {
	dp, err := o.fetchDeltaPageWithRetry(ctx, token, page)
	if err != nil {
		switch {
		case errors.Is(err, graph.ErrGone):
			return windowPage{}, syncdomain.NewDeltaExpiredError(err)
		case errors.Is(err, graph.ErrDeltaTokenNotALink):
			// A saved token that is not a link cannot be continued; read as
			// no token it took a full enumeration for the changes since it.
			return windowPage{}, syncdomain.NewCursorUnusableError("the saved position is not a link", err)
		}

		return windowPage{}, fmt.Errorf("sync: fetching delta page %d: %w", page, err)
	}

	items := 0

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
		return windowPage{raw: dp.Items, items: items, next: dp.DeltaLink, done: true, path: dp.Path}, nil
	}

	if dp.NextLink == "" {
		return windowPage{}, fmt.Errorf("sync: delta page %d has neither NextLink nor DeltaLink", page)
	}

	return windowPage{raw: dp.Items, items: items, next: dp.NextLink, path: dp.Path, nextPath: dp.NextPath}, nil
}

// classifyWindow resolves the ancestors the whole window still lacks, then
// classifies every item. The GetItem budget is spent only on items no page of
// the window and no baseline row can place (R-2.15.4).
func (o *RemoteObserver) classifyWindow(
	ctx context.Context, window []graph.Item, inflight map[string]inflightParent,
) []syncdomain.RemoteItem {
	o.Converter.resolveMissingAncestors(ctx, window, inflight)

	var events []syncdomain.RemoteItem
	for i := range window {
		if ev := o.Converter.ClassifyItem(&window[i], inflight); ev != nil {
			events = append(events, *ev)
		}
	}

	return events
}

// WaitText renders a retry delay or interval in human units for console
// messages ("8 s", "5 min", "1 h"), never a raw Go duration.
func WaitText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Round(time.Second)/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Round(time.Minute)/time.Minute))
	default:
		return fmt.Sprintf("%d h", int(d.Round(time.Hour)/time.Hour))
	}
}

// PollOutageLog reports a polling loop's failures once per outage instead of
// once per attempt. The first failure after a success is a Warn with the plain
// reason and the retry delay; later attempts in the same outage are Debug; the
// first success after an outage is one Notice, which the default console shows
// like the warning, so a terminal that saw the outage also sees it end. A loop
// owns its PollOutageLog and only that loop's goroutine touches it, so it needs
// no lock.
type PollOutageLog struct {
	failing bool
}

func (l *PollOutageLog) Failed(logger *slog.Logger, msg string, err error, delay time.Duration, attrs ...any) {
	if logger == nil {
		return
	}

	fields := append([]any{
		slog.Any("error", err),
		slog.String("retry_in", WaitText(delay)),
	}, attrs...)
	if l.failing {
		logger.Debug(msg, fields...)
		return
	}

	l.failing = true
	logger.Warn(msg, fields...)
}

func (l *PollOutageLog) Recovered(ctx context.Context, logger *slog.Logger, msg string) {
	if !l.failing {
		return
	}

	l.failing = false
	if logger != nil {
		logger.Log(ctx, loglevel.Notice, msg)
	}
}

// ---------------------------------------------------------------------------
// Pure helper functions
// ---------------------------------------------------------------------------

// classifyItemType determines the ItemType from graph.Item flags.
func classifyItemType(item *graph.Item) syncdomain.ItemType {
	switch {
	case item.IsRoot:
		return syncdomain.ItemTypeRoot
	case item.IsFolder:
		return syncdomain.ItemTypeFolder
	default:
		return syncdomain.ItemTypeFile
	}
}

// deltaPageRetryAfterLimit bounds the Retry-After a page fetch waits out in
// place. A longer pause is the caller's: the watch loop backs off its poll,
// a one-shot run ends and reports that OneDrive is limiting requests.
const deltaPageRetryAfterLimit = 2 * time.Minute

// fetchDeltaPageWithRetry fetches one delta page, retrying a transient server
// answer (5xx, a 429 with a short Retry-After, a dropped connection) a few
// times before the error surfaces. The sync client set dispatches once by
// design, since the engine owns retries for actions; a page of the change
// list is observation, not an action, and one 504 on page 2 of 32 used to
// fail a whole one-shot run (fault round of 2026-11-03) (R-2.15.5).
func (o *RemoteObserver) fetchDeltaPageWithRetry(ctx context.Context, token string, page int) (*graph.DeltaPage, error) {
	policy := retry.DeltaPagePolicy()
	for attempt := 0; ; attempt++ {
		dp, err := o.fetcher.Delta(ctx, o.driveID, token)
		if err == nil {
			return dp, nil
		}
		wait, retryable := deltaPageRetryDelay(err, policy, attempt)
		if !retryable || syncdomain.RunEnded(ctx, err) {
			return nil, fmt.Errorf("delta page %d: %w", page, err)
		}
		o.logger.Info("OneDrive did not answer a page of the change list; retrying",
			slog.Int("page", page),
			slog.Int("attempt", attempt+1),
			slog.Int("max_attempts", policy.MaxAttempts),
			slog.Duration("wait", wait.Round(time.Second)),
			slog.Any("error", err),
		)
		if sleepErr := o.clock.Sleep(ctx, wait); sleepErr != nil {
			return nil, fmt.Errorf("delta page %d (wait interrupted: %w): %w", page, sleepErr, err)
		}
	}
}

// deltaPageRetryDelay says whether a page fetch error is worth another try
// and how long to wait first: a 429 waits its Retry-After when that is within
// deltaPageRetryAfterLimit, a 5xx or a transport failure waits the policy's
// backoff. A 4xx other than 429 and an expired cursor are not retried. A
// request that timed out is a transport failure like any other: whether the
// run is ending is the caller's question (RunEnded), never the error's.
func deltaPageRetryDelay(err error, policy retry.Policy, attempt int) (time.Duration, bool) {
	if attempt+1 >= policy.MaxAttempts {
		return 0, false
	}
	var graphErr *graph.GraphError
	if errors.As(err, &graphErr) {
		switch {
		case errors.Is(err, graph.ErrThrottled):
			if graphErr.RetryAfter > deltaPageRetryAfterLimit {
				return 0, false
			}
			if graphErr.RetryAfter > 0 {
				return graphErr.RetryAfter, true
			}

			return policy.Delay(attempt), true
		case errors.Is(err, graph.ErrServerError):
			return policy.Delay(attempt), true
		default:
			return 0, false
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return policy.Delay(attempt), true
	}

	return 0, false
}
