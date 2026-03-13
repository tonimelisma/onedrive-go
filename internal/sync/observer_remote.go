package sync

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
)

// Constants for the remote observer (satisfy mnd linter).
const (
	maxDeltaPaginationGuard = 10000
	maxPathDepth            = 256
	minPollInterval         = 30 * time.Second
	specialFolderVault      = "vault" // Graph API personal vault folder name
)

// RemoteObserver transforms Graph API delta responses into []ChangeEvent.
// It handles pagination, path materialization, change classification, and
// normalization (NFC, driveID zero-padding). Delegates item conversion to
// an embedded itemConverter configured for primary drive observation.
type RemoteObserver struct {
	fetcher   DeltaFetcher
	converter *itemConverter
	logger    *slog.Logger
	driveID   driveid.ID
	sleepFunc func(ctx context.Context, d time.Duration) error
	obsWriter ObservationWriter // nil-safe: when set, observations are committed atomically with delta token

	// mu protects deltaToken for concurrent reads via CurrentDeltaToken().
	mu         stdsync.Mutex
	deltaToken string

	// lastActivityNano tracks the most recent successful poll time as Unix
	// nanoseconds. Used by the engine to detect stalled observers (B-125).
	lastActivityNano atomic.Int64

	// stats tracks observer-level metrics (B-127).
	stats observerCounters
}

// observerCounters holds atomic counters for observer metrics (B-127).
type observerCounters struct {
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
func NewRemoteObserver(fetcher DeltaFetcher, baseline *Baseline, driveID driveid.ID, logger *slog.Logger) *RemoteObserver {
	obs := &RemoteObserver{
		fetcher:   fetcher,
		logger:    logger,
		driveID:   driveID,
		sleepFunc: timeSleep,
	}
	obs.converter = newPrimaryConverter(baseline, driveID, logger, &obs.stats)

	return obs
}

// FullDelta fetches all delta pages and returns the accumulated change events
// plus the new delta token (DeltaLink URL) for the next sync pass.
func (o *RemoteObserver) FullDelta(ctx context.Context, savedToken string) ([]ChangeEvent, string, error) {
	o.logger.Info("remote observer starting delta enumeration",
		slog.String("drive_id", o.driveID.String()),
		slog.Bool("has_token", savedToken != ""),
	)

	var events []ChangeEvent
	inflight := make(map[driveid.ItemKey]inflightParent)
	token := savedToken

	for page := range maxDeltaPaginationGuard {
		pageEvents, newToken, done, err := o.fetchPage(ctx, token, page, inflight)
		if err != nil {
			o.stats.errors.Add(1)

			return nil, "", err
		}

		events = append(events, pageEvents...)

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
func (o *RemoteObserver) Watch(ctx context.Context, savedToken string, events chan<- ChangeEvent, interval time.Duration) error {
	if interval < minPollInterval {
		o.logger.Warn("poll interval below minimum, clamping",
			slog.Duration("requested", interval),
			slog.Duration("minimum", minPollInterval),
		)

		interval = minPollInterval
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

			if errors.Is(err, ErrDeltaExpired) {
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

				if sleepErr := o.sleepFunc(ctx, delay); sleepErr != nil {
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

			if sleepErr := o.sleepFunc(ctx, interval); sleepErr != nil {
				return nil
			}

			continue
		}

		// Commit observations atomically with delta token before sending
		// events to the channel. This ensures remote state is durable even
		// if the engine crashes before processing the events.
		if o.obsWriter != nil {
			observed := changeEventsToObservedItems(polledEvents)
			if commitErr := o.obsWriter.CommitObservation(ctx, observed, newToken, o.driveID); commitErr != nil {
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
		if sleepErr := o.sleepFunc(ctx, interval); sleepErr != nil {
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

// timeSleep waits for the given duration or until the context is canceled.
// Shared by RemoteObserver, LocalObserver, and ExecutorConfig as the default
// sleep implementation. Each injects it via their sleepFunc field for testability.
func timeSleep(ctx context.Context, d time.Duration) error {
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
	ctx context.Context, token string, page int, inflight map[driveid.ItemKey]inflightParent,
) ([]ChangeEvent, string, bool, error) {
	dp, err := o.fetcher.Delta(ctx, o.driveID, token)
	if err != nil {
		if errors.Is(err, graph.ErrGone) {
			return nil, "", false, ErrDeltaExpired
		}

		return nil, "", false, fmt.Errorf("sync: fetching delta page %d: %w", page, err)
	}

	// Pass 1: register all items in inflight so parent-chain walks
	// in pass 2 see every item regardless of arrival order.
	for i := range dp.Items {
		o.converter.registerInflight(&dp.Items[i], inflight)
	}

	// Pass 2: classify and emit events (inflight map is fully populated).
	var events []ChangeEvent

	for i := range dp.Items {
		if ev := o.converter.classifyItem(&dp.Items[i], inflight); ev != nil {
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
