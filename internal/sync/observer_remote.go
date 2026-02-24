package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"
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
	maxObserverPages = 10000
	maxPathDepth     = 256
)

// inflightParent tracks a non-root item seen in the current delta batch,
// allowing children later in the same batch to materialize paths before
// the baseline is updated.
type inflightParent struct {
	name          string
	parentID      string
	parentDriveID driveid.ID // drive containing this item's parent
	isRoot        bool
}

// RemoteObserver transforms Graph API delta responses into []ChangeEvent.
// It handles pagination, path materialization, change classification, and
// normalization (NFC, driveID zero-padding).
type RemoteObserver struct {
	fetcher  DeltaFetcher
	baseline *Baseline
	driveID  driveid.ID
	logger   *slog.Logger
}

// NewRemoteObserver creates a RemoteObserver for the given drive. The
// baseline must be a loaded Baseline (from BaselineManager.Load); it is
// read-only during observation. The caller must pass a normalized driveid.ID.
func NewRemoteObserver(fetcher DeltaFetcher, baseline *Baseline, driveID driveid.ID, logger *slog.Logger) *RemoteObserver {
	return &RemoteObserver{
		fetcher:  fetcher,
		baseline: baseline,
		driveID:  driveID,
		logger:   logger,
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
			return nil, "", err
		}

		events = append(events, pageEvents...)

		if done {
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
	key := driveid.NewItemKey(itemDriveID, item.ID)
	inflight[key] = inflightParent{
		name:          nfcNormalize(item.Name),
		parentID:      item.ParentID,
		parentDriveID: resolveParentDriveID(item, itemDriveID),
		isRoot:        item.IsRoot,
	}

	if item.IsRoot {
		o.logger.Debug("skipping root item", slog.String("item_id", item.ID))

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

// selectHash returns QuickXorHash if available, SHA256Hash as fallback,
// or empty string if neither is present.
func selectHash(item *graph.Item) string {
	if item.QuickXorHash != "" {
		return item.QuickXorHash
	}

	return item.SHA256Hash
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
