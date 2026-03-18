package syncobserve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	stdsync "sync"
	"sync/atomic"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/retry"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// Constants for the remote observer (satisfy mnd linter).
const (
	maxDeltaPaginationGuard = 10000
	maxPathDepth            = 256
	MinPollInterval         = 30 * time.Second
	specialFolderVault      = "vault" // Graph API personal vault folder name
)

// RemoteObserver transforms Graph API delta responses into []synctypes.ChangeEvent.
// It handles pagination, path materialization, change classification, and
// normalization (NFC, driveID zero-padding). Delegates item conversion to
// an embedded ItemConverter configured for primary drive observation.
type RemoteObserver struct {
	fetcher   synctypes.DeltaFetcher
	Converter *ItemConverter
	logger    *slog.Logger
	driveID   driveid.ID
	SleepFunc func(ctx context.Context, d time.Duration) error
	ObsWriter synctypes.ObservationWriter // nil-safe: when set, observations are committed atomically with delta token

	// mu protects deltaToken for concurrent reads via CurrentDeltaToken().
	mu         stdsync.Mutex
	deltaToken string

	// lastActivityNano tracks the most recent successful poll time as Unix
	// nanoseconds. Used by the engine to detect stalled observers (B-125).
	lastActivityNano atomic.Int64

	// stats tracks observer-level metrics (B-127).
	stats ObserverCounters
}

// ObserverCounters holds atomic counters for observer metrics (B-127).
type ObserverCounters struct {
	eventsEmitted  atomic.Int64
	pollsCompleted atomic.Int64
	errors         atomic.Int64
	hashesComputed atomic.Int64 // items with non-empty content hash (B-282)
}

// ObserverStats is a snapshot of observer metrics returned by Stats() (B-127).
type ObserverStats struct {
	EventsEmitted  int64
	PollsCompleted int64
	Errors         int64
	HashesComputed int64 // items processed with a content hash (B-282)
}

// NewRemoteObserver creates a RemoteObserver for the given drive. The
// baseline must be a loaded Baseline (from SyncStore.Load); it is
// read-only during observation. The caller must pass a normalized driveid.ID.
func NewRemoteObserver(
	fetcher synctypes.DeltaFetcher, baseline *synctypes.Baseline, driveID driveid.ID, logger *slog.Logger,
) *RemoteObserver {
	obs := &RemoteObserver{
		fetcher:   fetcher,
		logger:    logger,
		driveID:   driveID,
		SleepFunc: TimeSleep,
	}
	obs.Converter = NewPrimaryConverter(baseline, driveID, logger, &obs.stats)

	return obs
}

// SetObsWriter sets the observation writer for committing delta observations
// atomically with the delta token during watch mode. When non-nil, each poll
// cycle commits observations before returning events to the engine.
func (o *RemoteObserver) SetObsWriter(w synctypes.ObservationWriter) {
	o.ObsWriter = w
}

// FullDelta fetches all delta pages and returns the accumulated change events
// plus the new delta token (DeltaLink URL) for the next sync pass.
func (o *RemoteObserver) FullDelta(ctx context.Context, savedToken string) ([]synctypes.ChangeEvent, string, error) {
	o.logger.Info("remote observer starting delta enumeration",
		slog.String("drive_id", o.driveID.String()),
		slog.Bool("has_token", savedToken != ""),
	)

	var events []synctypes.ChangeEvent
	inflight := make(map[driveid.ItemKey]InflightParent)
	token := savedToken
	lastProgressLog := time.Now()

	for page := range maxDeltaPaginationGuard {
		pageEvents, newToken, done, err := o.fetchPage(ctx, token, page, inflight)
		if err != nil {
			o.stats.errors.Add(1)

			return nil, "", err
		}

		events = append(events, pageEvents...)

		// Periodic progress logging for long-running enumerations (100K+ item
		// drives can take minutes). Time-based (30s) rather than page-based to
		// produce evenly-spaced logs regardless of page size or API latency.
		if !done && time.Since(lastProgressLog) >= 30*time.Second {
			o.logger.Info("remote observer delta enumeration in progress",
				slog.Int("pages_fetched", page+1),
				slog.Int("events_so_far", len(events)),
			)

			lastProgressLog = time.Now()
		}

		if done {
			o.recordActivity()
			o.stats.pollsCompleted.Add(1)
			o.stats.eventsEmitted.Add(int64(len(events)))

			o.logger.Info("remote observer completed delta enumeration",
				slog.Int("pages", page+1),
				slog.Int("events", len(events)),
			)

			return events, newToken, nil
		}

		token = newToken
	}

	return nil, "", fmt.Errorf("sync: exceeded maximum page count (%d)", maxDeltaPaginationGuard)
}

// Watch continuously polls for remote delta changes and sends events to the
// provided channel. It blocks until the context is canceled, returning nil.
// On transient errors, it applies exponential backoff (starting at 5s, capped
// at the poll interval). On ErrDeltaExpired (410), it resets the token and
// retries with a full resync. The delta token is tracked internally — use
// CurrentDeltaToken() to read the latest value.
func (o *RemoteObserver) Watch(ctx context.Context, savedToken string, events chan<- synctypes.ChangeEvent, interval time.Duration) error {
	if interval < MinPollInterval {
		o.logger.Warn("poll interval below minimum, clamping",
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

	bo := retry.NewBackoff(retry.WatchRemote)
	bo.SetMaxOverride(interval)

	for {
		token := o.CurrentDeltaToken()

		polledEvents, newToken, err := o.FullDelta(ctx, token)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}

			if errors.Is(err, synctypes.ErrDeltaExpired) {
				o.logger.Warn("delta token expired during watch, resetting for full resync")
				o.setDeltaToken("")
				bo.Reset()
			} else {
				delay := bo.Next()
				o.logger.Warn("remote watch poll failed, backing off",
					slog.String("error", err.Error()),
					slog.Duration("backoff", delay),
					slog.Int("consecutive_errors", bo.Consecutive()),
				)

				if sleepErr := o.SleepFunc(ctx, delay); sleepErr != nil {
					return nil
				}
			}

			continue
		}

		// Skip token advancement and observation commit when 0 events are
		// returned. The old token replays the same empty window at zero cost,
		// but avoids advancing past deletions still propagating through the
		// Graph change log (ci_issues.md §20).
		if len(polledEvents) == 0 {
			o.logger.Debug("watch: delta returned 0 events, skipping token advancement")

			bo.Reset()

			if sleepErr := o.SleepFunc(ctx, interval); sleepErr != nil {
				return nil
			}

			continue
		}

		// Commit observations atomically with delta token before sending
		// events to the channel. This ensures remote state is durable even
		// if the engine crashes before processing the events.
		if o.ObsWriter != nil {
			observed := changeEventsToObservedItems(o.logger, polledEvents)
			if commitErr := o.ObsWriter.CommitObservation(ctx, observed, newToken, o.driveID); commitErr != nil {
				o.logger.Error("failed to commit observations in watch",
					slog.String("error", commitErr.Error()),
					slog.Int("events", len(polledEvents)),
				)
				// Retry on next poll — token replay is idempotent.
				continue
			}
		}

		// Successful poll — send events, advance token, reset backoff.
		// Blocking send: remote delta events must not be dropped. Unlike local
		// events (which the safety scan recovers), dropped remote events would
		// advance the delta token past unprocessed changes — silent data loss
		// with no recovery mechanism. Backpressure here correctly slows polling.
		for i := range polledEvents {
			select {
			case events <- polledEvents[i]:
			case <-ctx.Done():
				return nil
			}
		}

		o.setDeltaToken(newToken)
		bo.Reset()

		// Wait for the next poll interval.
		if sleepErr := o.SleepFunc(ctx, interval); sleepErr != nil {
			return nil
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

// LastActivity returns the time of the most recent successful poll.
// Returns zero time if no poll has completed. Thread-safe — can be called
// from any goroutine while Watch() is running (B-125).
func (o *RemoteObserver) LastActivity() time.Time {
	nano := o.lastActivityNano.Load()
	if nano == 0 {
		return time.Time{}
	}

	return time.Unix(0, nano)
}

// recordActivity updates the liveness timestamp to now.
func (o *RemoteObserver) recordActivity() {
	o.lastActivityNano.Store(time.Now().UnixNano())
}

// Stats returns a snapshot of observer metrics. Thread-safe (B-127).
func (o *RemoteObserver) Stats() ObserverStats {
	return ObserverStats{
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

// TimeSleep waits for the given duration or until the context is canceled.
// Shared by RemoteObserver, LocalObserver, and ExecutorConfig as the default
// sleep implementation. Each injects it via their sleepFunc field for testability.
func TimeSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// fetchPage fetches a single delta page, processes items, and returns events.
// Returns done=true with the DeltaLink when the final page is reached, or
// done=false with NextLink for continued pagination.
//
// Two-pass processing (B-281): the Graph API does not guarantee parent-before-
// child ordering within a delta page. Pass 1 registers ALL items in the
// inflight map so that pass 2 can correctly classify vault descendants even
// when the child appears before its vault parent in the response.
func (o *RemoteObserver) fetchPage(
	ctx context.Context, token string, page int, inflight map[driveid.ItemKey]InflightParent,
) ([]synctypes.ChangeEvent, string, bool, error) {
	dp, err := o.fetcher.Delta(ctx, o.driveID, token)
	if err != nil {
		if errors.Is(err, graph.ErrGone) {
			return nil, "", false, synctypes.ErrDeltaExpired
		}

		return nil, "", false, fmt.Errorf("sync: fetching delta page %d: %w", page, err)
	}

	// Pass 1: register all items in inflight so parent-chain walks
	// in pass 2 see every item regardless of arrival order.
	for i := range dp.Items {
		o.Converter.registerInflight(&dp.Items[i], inflight)
	}

	// Pass 2: classify and emit events (inflight map is fully populated).
	var events []synctypes.ChangeEvent

	for i := range dp.Items {
		if ev := o.Converter.ClassifyItem(&dp.Items[i], inflight); ev != nil {
			events = append(events, *ev)
		}
	}

	if dp.DeltaLink != "" {
		return events, dp.DeltaLink, true, nil
	}

	if dp.NextLink == "" {
		return nil, "", false, fmt.Errorf("sync: delta page %d has neither NextLink nor DeltaLink", page)
	}

	return events, dp.NextLink, false, nil
}

// ---------------------------------------------------------------------------
// Pure helper functions
// ---------------------------------------------------------------------------

// ClassifyItemType determines the synctypes.ItemType from graph.Item flags.
func ClassifyItemType(item *graph.Item) synctypes.ItemType {
	switch {
	case item.IsRoot:
		return synctypes.ItemTypeRoot
	case item.IsFolder:
		return synctypes.ItemTypeFolder
	default:
		return synctypes.ItemTypeFile
	}
}

// ToUnixNano converts a time.Time to Unix nanoseconds. Returns 0 for
// the zero time value.
func ToUnixNano(t time.Time) int64 {
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

// changeEventsToObservedItems converts remote ChangeEvents into ObservedItems
// for CommitObservation. Filters out local-source events and events with
// empty ItemIDs (defensive guard against malformed API responses).
func changeEventsToObservedItems(logger *slog.Logger, events []synctypes.ChangeEvent) []synctypes.ObservedItem {
	var items []synctypes.ObservedItem

	for i := range events {
		if events[i].Source != synctypes.SourceRemote {
			continue
		}

		if events[i].ItemID == "" {
			logger.Warn("changeEventsToObservedItems: skipping event with empty ItemID",
				slog.String("path", events[i].Path),
			)

			continue
		}

		items = append(items, synctypes.ObservedItem{
			DriveID:   events[i].DriveID,
			ItemID:    events[i].ItemID,
			ParentID:  events[i].ParentID,
			Path:      events[i].Path,
			ItemType:  events[i].ItemType,
			Hash:      events[i].Hash,
			Size:      events[i].Size,
			Mtime:     events[i].Mtime,
			ETag:      events[i].ETag,
			IsDeleted: events[i].IsDeleted,
		})
	}

	return items
}
