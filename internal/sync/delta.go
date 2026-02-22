package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// defaultBatchSize is the number of items accumulated before flushing to the store.
// Balances memory usage against write amplification from frequent DB commits.
const defaultBatchSize = 500

// errDeltaTokenExpired is a sentinel used internally to signal that the delta
// token has expired (HTTP 410) and a full re-enumeration is needed. The caller
// (FetchAndApply) retries with an empty token when it sees this error.
var errDeltaTokenExpired = errors.New("delta token expired")

// DeltaProcessor fetches remote changes from the Graph API and applies them
// to the sync state database. It implements the algorithm from
// sync-algorithm.md sections 3.1-3.6: fetch pages, normalize, reorder
// deletions, apply to DB, handle HTTP 410, and track delta completeness.
type DeltaProcessor struct {
	fetcher DeltaFetcher
	store   DeltaStore
	logger  *slog.Logger
}

// NewDeltaProcessor creates a DeltaProcessor that fetches remote changes
// via fetcher and persists state via store.
func NewDeltaProcessor(fetcher DeltaFetcher, store DeltaStore, logger *slog.Logger) *DeltaProcessor {
	return &DeltaProcessor{
		fetcher: fetcher,
		store:   store,
		logger:  logger,
	}
}

// FetchAndApply is the main entry point for delta processing. It fetches all
// remote changes for the given drive and applies them to the state database.
// On HTTP 410 (expired token), it deletes the token and re-enumerates from scratch.
func (dp *DeltaProcessor) FetchAndApply(ctx context.Context, driveID string) error {
	dp.logger.Info("starting delta fetch", slog.String("drive_id", driveID))

	token, err := dp.store.GetDeltaToken(ctx, driveID)
	if err != nil {
		return fmt.Errorf("get delta token: %w", err)
	}

	dp.logger.Debug("loaded delta token",
		slog.String("drive_id", driveID),
		slog.Bool("initial_sync", token == ""),
	)

	complete, err := dp.fetchPages(ctx, driveID, token)
	if errors.Is(err, errDeltaTokenExpired) {
		// Token expired — retry with empty token (full re-enumeration).
		complete, err = dp.fetchPages(ctx, driveID, "")
	}

	if err != nil {
		return err
	}

	if setErr := dp.store.SetDeltaComplete(ctx, driveID, complete); setErr != nil {
		return fmt.Errorf("set delta complete: %w", setErr)
	}

	dp.logger.Info("delta fetch finished",
		slog.String("drive_id", driveID),
		slog.Bool("complete", complete),
	)

	return nil
}

// fetchPages loops through delta pages, accumulating items into batches and
// flushing when the batch size threshold is reached. Returns true if the
// delta response was complete (received a deltaLink).
func (dp *DeltaProcessor) fetchPages(ctx context.Context, driveID, token string) (bool, error) {
	var batch []*Item

	for {
		page, err := dp.fetcher.Delta(ctx, driveID, token)
		if err != nil {
			return false, dp.handleFetchError(ctx, driveID, err)
		}

		batch = dp.accumulateItems(batch, page.Items, driveID)

		if len(batch) >= defaultBatchSize {
			if flushErr := dp.flushBatch(ctx, driveID, batch); flushErr != nil {
				return false, flushErr
			}

			batch = batch[:0]
		}

		// deltaLink present means this was the final page.
		if page.DeltaLink != "" {
			return dp.finalizeDelta(ctx, driveID, page.DeltaLink, batch)
		}

		// Follow pagination via nextLink.
		token = page.NextLink
	}
}

// handleFetchError checks whether the error is HTTP 410 (expired token)
// and prepares for a full re-enumeration by deleting the token and marking
// delta as incomplete. Returns errDeltaTokenExpired so the caller can retry.
// Other errors are returned as-is.
func (dp *DeltaProcessor) handleFetchError(ctx context.Context, driveID string, err error) error {
	if !errors.Is(err, graph.ErrGone) {
		return fmt.Errorf("delta fetch: %w", err)
	}

	dp.logger.Warn("delta token expired (HTTP 410), re-enumerating",
		slog.String("drive_id", driveID),
	)

	if delErr := dp.store.DeleteDeltaToken(ctx, driveID); delErr != nil {
		return fmt.Errorf("delete expired delta token: %w", delErr)
	}

	if completeErr := dp.store.SetDeltaComplete(ctx, driveID, false); completeErr != nil {
		return fmt.Errorf("set delta incomplete after 410: %w", completeErr)
	}

	return errDeltaTokenExpired
}

// accumulateItems converts graph items from a single page and appends them
// to the running batch. Skips packages (OneNote) per sync-algorithm.md §3.2.
func (dp *DeltaProcessor) accumulateItems(batch []*Item, graphItems []graph.Item, driveID string) []*Item {
	for i := range graphItems {
		item := convertGraphItem(&graphItems[i], driveID)
		if item == nil {
			continue // skipped (e.g., OneNote package)
		}

		batch = append(batch, item)
	}

	return batch
}

// finalizeDelta flushes any remaining items and saves the delta token.
// Returns (true, nil) on success indicating the delta was complete.
func (dp *DeltaProcessor) finalizeDelta(ctx context.Context, driveID, deltaLink string, batch []*Item) (bool, error) {
	if len(batch) > 0 {
		if err := dp.flushBatch(ctx, driveID, batch); err != nil {
			return false, err
		}
	}

	if err := dp.store.SaveDeltaToken(ctx, driveID, deltaLink); err != nil {
		return false, fmt.Errorf("save delta token: %w", err)
	}

	dp.logger.Info("delta token saved",
		slog.String("drive_id", driveID),
	)

	return true, nil
}

// flushBatch reorders deletions ahead of non-deletions and applies the
// batch to the store. See sync-algorithm.md §3.3 for rationale.
func (dp *DeltaProcessor) flushBatch(ctx context.Context, driveID string, items []*Item) error {
	dp.logger.Debug("flushing batch",
		slog.String("drive_id", driveID),
		slog.Int("count", len(items)),
	)

	reorderDeletions(items)

	return dp.applyBatch(ctx, driveID, items)
}

// applyBatch applies each item in the batch to the state database
// using the decision tree from sync-algorithm.md §3.4.
func (dp *DeltaProcessor) applyBatch(ctx context.Context, driveID string, items []*Item) error {
	for _, item := range items {
		if err := dp.applyDeltaItem(ctx, item); err != nil {
			return fmt.Errorf("apply delta item %s/%s: %w", driveID, item.ItemID, err)
		}
	}

	return nil
}

// applyDeltaItem implements the per-item decision tree from
// sync-algorithm.md §3.4: tombstone, new item, resurrection, or update.
func (dp *DeltaProcessor) applyDeltaItem(ctx context.Context, item *Item) error {
	existing, err := dp.store.GetItem(ctx, item.DriveID, item.ItemID)
	if err != nil {
		return fmt.Errorf("get existing item: %w", err)
	}

	if item.IsDeleted {
		return dp.applyDeletion(ctx, existing, item)
	}

	if existing == nil {
		return dp.applyNewItem(ctx, item)
	}

	if existing.IsDeleted {
		return dp.applyResurrection(ctx, existing, item)
	}

	return dp.applyUpdate(ctx, existing, item)
}

// applyDeletion handles a deleted item from the delta response.
// Skips if the item is already gone or already tombstoned.
func (dp *DeltaProcessor) applyDeletion(ctx context.Context, existing, incoming *Item) error {
	if existing == nil {
		dp.logger.Debug("skipping deletion for unknown item",
			slog.String("item_id", incoming.ItemID),
		)

		return nil
	}

	if existing.IsDeleted {
		dp.logger.Debug("skipping deletion for already-tombstoned item",
			slog.String("item_id", incoming.ItemID),
		)

		return nil
	}

	dp.logger.Info("marking item deleted",
		slog.String("drive_id", incoming.DriveID),
		slog.String("item_id", incoming.ItemID),
		slog.String("name", existing.Name),
	)

	return dp.store.MarkDeleted(ctx, incoming.DriveID, incoming.ItemID, NowNano())
}

// applyNewItem handles an item that does not exist in the state database.
// Materializes the path from the parent chain and inserts.
func (dp *DeltaProcessor) applyNewItem(ctx context.Context, item *Item) error {
	path, err := dp.store.MaterializePath(ctx, item.DriveID, item.ItemID)
	if err != nil {
		return fmt.Errorf("materialize path for new item: %w", err)
	}

	item.Path = path

	dp.logger.Info("inserting new item",
		slog.String("drive_id", item.DriveID),
		slog.String("item_id", item.ItemID),
		slog.String("path", item.Path),
	)

	return dp.store.UpsertItem(ctx, item)
}

// applyResurrection handles a previously-tombstoned item that reappears
// in the delta response. Clears the tombstone and updates remote fields.
func (dp *DeltaProcessor) applyResurrection(ctx context.Context, existing, incoming *Item) error {
	existing.IsDeleted = false
	existing.DeletedAt = nil

	updateRemoteFields(existing, incoming)

	path, err := dp.store.MaterializePath(ctx, existing.DriveID, existing.ItemID)
	if err != nil {
		return fmt.Errorf("materialize path for resurrected item: %w", err)
	}

	existing.Path = path
	existing.UpdatedAt = NowNano()

	dp.logger.Info("resurrecting tombstoned item",
		slog.String("drive_id", existing.DriveID),
		slog.String("item_id", existing.ItemID),
		slog.String("path", existing.Path),
	)

	return dp.store.UpsertItem(ctx, existing)
}

// applyUpdate handles an existing live item whose remote state has changed.
// Detects move/rename by comparing parent and name, and cascades path
// updates for folder moves.
func (dp *DeltaProcessor) applyUpdate(ctx context.Context, existing, incoming *Item) error {
	oldParentID := existing.ParentID
	oldName := existing.Name
	oldPath := existing.Path

	updateRemoteFields(existing, incoming)
	existing.UpdatedAt = NowNano()

	// Detect move or rename: parent or name changed.
	if incoming.ParentID != oldParentID || incoming.Name != oldName {
		return dp.applyMoveOrRename(ctx, existing, oldPath)
	}

	return dp.store.UpsertItem(ctx, existing)
}

// applyMoveOrRename re-materializes the path after a parent or name change
// and cascades path updates for folder moves.
func (dp *DeltaProcessor) applyMoveOrRename(ctx context.Context, existing *Item, oldPath string) error {
	newPath, err := dp.store.MaterializePath(ctx, existing.DriveID, existing.ItemID)
	if err != nil {
		return fmt.Errorf("materialize path for moved item: %w", err)
	}

	existing.Path = newPath

	dp.logger.Info("detected move/rename",
		slog.String("drive_id", existing.DriveID),
		slog.String("item_id", existing.ItemID),
		slog.String("old_path", oldPath),
		slog.String("new_path", newPath),
	)

	// Cascade path updates for all descendants when a folder moves.
	if existing.ItemType == ItemTypeFolder && oldPath != newPath {
		if err := dp.store.CascadePathUpdate(ctx, oldPath, newPath); err != nil {
			return fmt.Errorf("cascade path update: %w", err)
		}
	}

	return dp.store.UpsertItem(ctx, existing)
}

// convertGraphItem converts a graph.Item to a sync.Item. Returns nil for
// items that should be skipped (e.g., OneNote packages).
func convertGraphItem(gItem *graph.Item, driveID string) *Item {
	// Skip OneNote packages per sync-algorithm.md §3.2.
	if gItem.IsPackage {
		return nil
	}

	item := &Item{
		ItemID:        gItem.ID,
		Name:          gItem.Name,
		ParentID:      gItem.ParentID,
		ParentDriveID: gItem.ParentDriveID,
		ETag:          gItem.ETag,
		CTag:          gItem.CTag,
		QuickXorHash:  gItem.QuickXorHash,
		SHA256Hash:    gItem.SHA256Hash,
		IsDeleted:     gItem.IsDeleted,
		CreatedAt:     NowNano(),
		UpdatedAt:     NowNano(),
	}

	// Use drive ID from the graph item if present, otherwise use the
	// drive ID passed by the caller (the drive being synced).
	if gItem.DriveID != "" {
		item.DriveID = gItem.DriveID
	} else {
		item.DriveID = driveID
	}

	// Classify item type. Root detection uses the Graph API's root facet
	// (present only on the top-most drive item) so that MaterializePath
	// terminates without prepending the root's name to every path.
	switch {
	case gItem.IsFolder && gItem.IsRoot:
		item.ItemType = ItemTypeRoot
	case gItem.IsFolder:
		item.ItemType = ItemTypeFolder
	default:
		item.ItemType = ItemTypeFile
	}

	// Size is nullable for deleted Personal items (sync-algorithm.md §3.2).
	if gItem.Size > 0 || !gItem.IsDeleted {
		item.Size = Int64Ptr(gItem.Size)
	}

	// Convert remote modification time to Unix nanoseconds.
	remoteMtime := ToUnixNano(gItem.ModifiedAt)
	if remoteMtime != 0 {
		item.RemoteMtime = Int64Ptr(remoteMtime)
	}

	return item
}

// updateRemoteFields copies remote-side fields from incoming delta data
// onto an existing item. Local and synced fields are preserved.
func updateRemoteFields(existing, incoming *Item) {
	existing.Name = incoming.Name
	existing.ParentID = incoming.ParentID
	existing.ParentDriveID = incoming.ParentDriveID
	existing.ItemType = incoming.ItemType
	existing.Size = incoming.Size
	existing.ETag = incoming.ETag
	existing.CTag = incoming.CTag
	existing.QuickXorHash = incoming.QuickXorHash
	existing.SHA256Hash = incoming.SHA256Hash
	existing.RemoteMtime = incoming.RemoteMtime
}

// reorderDeletions partitions items in-place so that deleted items come
// before non-deleted items, preserving relative order within each group.
// This prevents the API ordering bug where a deletion arrives after a
// creation at the same path within a single page, which would cause the
// sync engine to create-then-delete (losing data).
// See sync-algorithm.md §3.3.
func reorderDeletions(items []*Item) {
	n := len(items)
	if n == 0 {
		return
	}

	// Two-pass stable partition: preserves relative ordering within
	// the deleted and non-deleted groups.
	tmp := make([]*Item, 0, n)

	for _, item := range items {
		if item.IsDeleted {
			tmp = append(tmp, item)
		}
	}

	for _, item := range items {
		if !item.IsDeleted {
			tmp = append(tmp, item)
		}
	}

	copy(items, tmp)
}
