package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// ErrDeltaExpired indicates the saved delta token has expired and a full
// resync is required. Returned when the Graph API responds with HTTP 410.
var ErrDeltaExpired = errors.New("sync: delta token expired (resync required)")

// Constants for the remote observer (satisfy mnd linter).
const (
	maxObserverPages      = 10000
	maxPathDepth          = 256
	initialWatchBackoff   = 5 * time.Second
	backoffMultiplier     = 2
	maxConsecutiveBackoff = 10 // cap prevents overflow; 5s * 2^10 = 5120s > any interval
	minPollInterval       = 30 * time.Second
)

// inflightParent tracks a non-root item seen in the current delta batch,
// allowing children later in the same batch to materialize paths before
// the baseline is updated.
type inflightParent struct {
	name          string
	parentID      string
	parentDriveID driveid.ID // drive containing this item's parent
	isRoot        bool
	isVault       bool // true for Personal Vault folder (B-271)
}

// RemoteObserver transforms Graph API delta responses into []ChangeEvent.
// It handles pagination, path materialization, change classification, and
// normalization (NFC, driveID zero-padding).
type RemoteObserver struct {
	fetcher   DeltaFetcher
	baseline  *Baseline
	driveID   driveid.ID
	logger    *slog.Logger
	sleepFunc func(ctx context.Context, d time.Duration) error

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
}

// ObserverStats is a snapshot of observer metrics returned by Stats() (B-127).
type ObserverStats struct {
	EventsEmitted  int64
	PollsCompleted int64
	Errors         int64
}

// NewRemoteObserver creates a RemoteObserver for the given drive. The
// baseline must be a loaded Baseline (from BaselineManager.Load); it is
// read-only during observation. The caller must pass a normalized driveid.ID.
func NewRemoteObserver(fetcher DeltaFetcher, baseline *Baseline, driveID driveid.ID, logger *slog.Logger) *RemoteObserver {
	return &RemoteObserver{
		fetcher:   fetcher,
		baseline:  baseline,
		driveID:   driveID,
		logger:    logger,
		sleepFunc: timeSleep,
	}
}

// FullDelta fetches all delta pages and returns the accumulated change events
// plus the new delta token (DeltaLink URL) for the next sync cycle.
func (o *RemoteObserver) FullDelta(ctx context.Context, savedToken string) ([]ChangeEvent, string, error) {
	o.logger.Info("remote observer starting delta enumeration",
		slog.String("drive_id", o.driveID.String()),
		slog.Bool("has_token", savedToken != ""),
	)

	var events []ChangeEvent
	inflight := make(map[driveid.ItemKey]inflightParent)
	token := savedToken

	for page := 0; page < maxObserverPages; page++ {
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

	return nil, "", fmt.Errorf("sync: exceeded maximum page count (%d)", maxObserverPages)
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

	backoff := initialWatchBackoff
	consecutiveErrors := 0

	for {
		token := o.CurrentDeltaToken()

		polledEvents, newToken, err := o.FullDelta(ctx, token)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}

			sleepDur := backoff
			backoff, consecutiveErrors = o.advanceBackoff(err, backoff, consecutiveErrors, interval)

			if sleepErr := o.sleepFunc(ctx, sleepDur); sleepErr != nil {
				return nil
			}

			continue
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

		backoff = initialWatchBackoff
		consecutiveErrors = 0

		// Wait for the next poll interval.
		if sleepErr := o.sleepFunc(ctx, interval); sleepErr != nil {
			return nil
		}
	}
}

// advanceBackoff processes a poll error, advancing the backoff for the next
// retry. The caller sleeps with the CURRENT backoff before calling this.
// Returns the updated backoff duration and consecutive error count.
func (o *RemoteObserver) advanceBackoff(
	err error, backoff time.Duration, consecutiveErrors int, interval time.Duration,
) (time.Duration, int) {
	if errors.Is(err, ErrDeltaExpired) {
		o.logger.Warn("delta token expired during watch, resetting for full resync")
		o.setDeltaToken("")

		return initialWatchBackoff, 0
	}

	consecutiveErrors++

	o.logger.Warn("remote watch poll failed, backing off",
		slog.String("error", err.Error()),
		slog.Duration("backoff", backoff),
		slog.Int("consecutive_errors", consecutiveErrors),
	)

	nextBackoff := backoff * backoffMultiplier
	if nextBackoff > interval {
		nextBackoff = interval
	}

	if consecutiveErrors > maxConsecutiveBackoff {
		nextBackoff = interval
	}

	return nextBackoff, consecutiveErrors
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

	var events []ChangeEvent
	for i := range dp.Items {
		if ev := o.processItem(&dp.Items[i], inflight); ev != nil {
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

// processItem converts a single graph.Item into a ChangeEvent, registering
// it in the inflight parent map for path materialization. Returns nil for
// root items (structural, not content changes).
func (o *RemoteObserver) processItem(item *graph.Item, inflight map[driveid.ItemKey]inflightParent) *ChangeEvent {
	itemDriveID := o.resolveItemDriveID(item)

	// Register in inflight map before classification so children in the
	// same batch can materialize paths through this item.
	isVault := item.SpecialFolderName == "vault"
	key := driveid.NewItemKey(itemDriveID, item.ID)
	inflight[key] = inflightParent{
		name:          nfcNormalize(item.Name),
		parentID:      item.ParentID,
		parentDriveID: resolveParentDriveID(item, itemDriveID),
		isRoot:        item.IsRoot,
		isVault:       isVault,
	}

	if item.IsRoot {
		o.logger.Debug("skipping root item", slog.String("item_id", item.ID))

		return nil
	}

	// Personal Vault exclusion (B-271): skip the vault folder itself and
	// any items whose parent chain includes a vault folder. This prevents
	// data loss from vault lock/unlock cycles where items appear and
	// disappear in delta responses.
	if isVault || o.isDescendantOfVault(item, inflight, itemDriveID) {
		o.logger.Info("skipping vault item",
			slog.String("item_id", item.ID),
			slog.String("name", item.Name),
		)

		return nil
	}

	return o.classifyAndConvert(item, inflight, itemDriveID)
}

// classifyAndConvert classifies the change type and builds a ChangeEvent.
func (o *RemoteObserver) classifyAndConvert(
	item *graph.Item, inflight map[driveid.ItemKey]inflightParent, itemDriveID driveid.ID,
) *ChangeEvent {
	name := nfcNormalize(item.Name)
	baselineKey := driveid.NewItemKey(itemDriveID, item.ID)
	existing, _ := o.baseline.GetByID(baselineKey)

	ev := ChangeEvent{
		Source:    SourceRemote,
		ItemID:    item.ID,
		ParentID:  item.ParentID,
		DriveID:   itemDriveID,
		ItemType:  classifyItemType(item),
		Name:      name,
		Size:      item.Size,
		Hash:      selectHash(item),
		Mtime:     toUnixNano(item.ModifiedAt),
		ETag:      item.ETag,
		CTag:      item.CTag,
		IsDeleted: item.IsDeleted,
	}

	switch {
	case item.IsDeleted:
		ev.Type = ChangeDelete
		// Business API: deleted items may lack Name.
		if ev.Name == "" && existing != nil {
			ev.Name = path.Base(existing.Path)
		}

		if existing != nil {
			ev.Path = existing.Path
		}

	case existing != nil:
		ev.Path = o.materializePath(item, inflight, itemDriveID)
		if ev.Path != existing.Path {
			ev.Type = ChangeMove
			ev.OldPath = existing.Path
		} else {
			ev.Type = ChangeModify
		}

	default:
		ev.Type = ChangeCreate
		ev.Path = o.materializePath(item, inflight, itemDriveID)
	}

	return &ev
}

// materializePath builds the full relative path by walking the parent chain.
// It checks the inflight map first (for items in the current delta batch),
// then the baseline. Stops at the drive root or when a baseline entry
// provides a shortcut.
func (o *RemoteObserver) materializePath(
	item *graph.Item, inflight map[driveid.ItemKey]inflightParent, itemDriveID driveid.ID,
) string {
	segments := []string{nfcNormalize(item.Name)}
	parentDriveID := resolveParentDriveID(item, itemDriveID)
	parentID := item.ParentID

	for depth := 0; depth < maxPathDepth; depth++ {
		if parentID == "" {
			break
		}

		parentKey := driveid.NewItemKey(parentDriveID, parentID)

		// Check inflight map first (items from current delta batch).
		if p, ok := inflight[parentKey]; ok {
			if p.isRoot {
				break
			}

			segments = append(segments, p.name)
			parentDriveID = p.parentDriveID
			parentID = p.parentID

			continue
		}

		// Baseline shortcut: prepend this item's full stored path.
		if entry, ok := o.baseline.GetByID(parentKey); ok && entry.Path != "" {
			slices.Reverse(segments)

			return entry.Path + "/" + strings.Join(segments, "/")
		}

		// Parent not found — orphaned item.
		o.logger.Warn("orphaned item: parent not found in inflight or baseline",
			slog.String("item_id", item.ID),
			slog.String("parent_id", parentID),
			slog.String("parent_drive_id", parentDriveID.String()),
		)

		break
	}

	slices.Reverse(segments)

	return strings.Join(segments, "/")
}

// isDescendantOfVault walks the parent chain in the inflight map to check
// whether any ancestor is a vault folder. Limited to maxPathDepth to prevent
// infinite loops in malformed data (B-271).
func (o *RemoteObserver) isDescendantOfVault(
	item *graph.Item, inflight map[driveid.ItemKey]inflightParent, itemDriveID driveid.ID,
) bool {
	parentDriveID := resolveParentDriveID(item, itemDriveID)
	parentID := item.ParentID

	for depth := 0; depth < maxPathDepth; depth++ {
		if parentID == "" {
			return false
		}

		parentKey := driveid.NewItemKey(parentDriveID, parentID)

		p, ok := inflight[parentKey]
		if !ok {
			return false
		}

		if p.isVault {
			return true
		}

		if p.isRoot {
			return false
		}

		parentDriveID = p.parentDriveID
		parentID = p.parentID
	}

	return false
}

// resolveItemDriveID returns the normalized driveID for an item, falling
// back to the observer's driveID when the item's DriveID is empty.
func (o *RemoteObserver) resolveItemDriveID(item *graph.Item) driveid.ID {
	if item.DriveID.IsZero() {
		return o.driveID
	}

	return item.DriveID
}

// resolveParentDriveID returns the normalized driveID for the parent of an
// item, handling cross-drive references (e.g. shared items).
func resolveParentDriveID(item *graph.Item, itemDriveID driveid.ID) driveid.ID {
	if !item.ParentDriveID.IsZero() {
		return item.ParentDriveID
	}

	return itemDriveID
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

// selectHash returns the best available content hash from the item, preferring
// QuickXorHash (most common), falling back to SHA256Hash, then SHA1Hash.
// Returns empty string if no hash is available — the caller must handle
// hash-less items appropriately (typically skipping verification) (B-021).
func selectHash(item *graph.Item) string {
	if item.QuickXorHash != "" {
		return item.QuickXorHash
	}

	if item.SHA256Hash != "" {
		return item.SHA256Hash
	}

	return item.SHA1Hash
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
