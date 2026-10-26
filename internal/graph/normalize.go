package graph

import (
	"log/slog"
	"slices"
)

// normalizeDeltaItems applies delta-specific quirk handling to a batch of
// items. The pipeline runs in a fixed order:
// 1. Clear bogus hashes on deleted items
// 2. Deduplicate items that appear multiple times (keep last occurrence)
// 3. Reorder so deletions at a parent are processed before creations
//
// Item names are opaque and pass through verbatim. An earlier stage
// URL-decoded every name; a live probe on Personal (create, GET, /children
// and /delta) returned `a%20b.txt`, `test%46.txt` and `x%2Fy.txt` literally,
// so decoding renamed user files and invented subfolders.
// parentReference.path is not encoded either (L-32; see items.go).
func normalizeDeltaItems(items []Item, logger *slog.Logger) []Item {
	items = clearDeletedHashes(items, logger)
	items = deduplicateItems(items, logger)
	items = reorderDeletions(items, logger)

	return items
}

// clearDeletedHashes clears all hash fields on deleted items. The Graph API
// sometimes returns stale or bogus hashes on deleted items in delta responses,
// which can cause spurious hash mismatches during sync processing.
func clearDeletedHashes(items []Item, logger *slog.Logger) []Item {
	for i := range items {
		if !items[i].IsDeleted {
			continue
		}

		if items[i].QuickXorHash != "" || items[i].SHA1Hash != "" || items[i].SHA256Hash != "" {
			logger.Debug("clearing bogus hashes on deleted item",
				slog.String("item_id", items[i].ID),
				slog.String("name", items[i].Name),
			)

			items[i].QuickXorHash = ""
			items[i].SHA1Hash = ""
			items[i].SHA256Hash = ""
		}
	}

	return items
}

// deduplicateItems removes duplicate item IDs, keeping only the last occurrence.
// The Graph API can return the same item multiple times in a single delta batch
// when it changes between pages — only the final state matters.
func deduplicateItems(items []Item, logger *slog.Logger) []Item {
	if len(items) == 0 {
		return items
	}

	// Reverse, then iterate forward to find the last occurrence of each ID
	// (which is now the first after reversal). This avoids backwards indexing
	// that triggers gosec G602 false positives.
	reversed := make([]Item, len(items))
	copy(reversed, items)
	slices.Reverse(reversed)

	seen := make(map[string]bool, len(reversed))
	kept := make([]Item, 0, len(reversed))

	for i := range reversed {
		if seen[reversed[i].ID] {
			logger.Debug("deduplicating item, keeping later occurrence",
				slog.String("item_id", reversed[i].ID),
				slog.String("name", reversed[i].Name),
			)

			continue
		}

		seen[reversed[i].ID] = true
		kept = append(kept, reversed[i])
	}

	// Reverse back to restore original ordering of kept items.
	slices.Reverse(kept)

	if dupes := len(items) - len(kept); dupes > 0 {
		logger.Info("deduplicated items in delta batch",
			slog.Int("duplicate_count", dupes),
			slog.Int("remaining_count", len(kept)),
		)
	}

	return kept
}

// reorderDeletions moves deletions ahead of non-deletions among items that
// share a parent, so a rename-then-recreate at the same parent does not fail
// with "item already exists".
//
// This is a per-parent partition rather than a sort. Expressing it as a sort
// requires a comparator that returns "equal" for items with different parents,
// which is not a valid ordering: a delete and a create at parent P compare as
// ordered, yet each compares equal to an unrelated item at parent Q, so
// transitivity fails. A stable sort given that comparator quietly does nothing
// whenever an item from another parent sits between the two, which in a delta
// batch spanning folders is the normal case rather than the exception.
//
// Items keep their slots: each parent's items are rewritten into the exact
// positions they already occupied, so the relative order of everything else is
// untouched.
func reorderDeletions(items []Item, logger *slog.Logger) []Item {
	if len(items) == 0 {
		return items
	}

	slotsByParent := make(map[string][]int, len(items))
	for i := range items {
		slotsByParent[items[i].ParentID] = append(slotsByParent[items[i].ParentID], i)
	}

	reordered := false

	for _, slots := range slotsByParent {
		if partitionDeletionsFirst(items, slots) {
			reordered = true
		}
	}

	if reordered {
		logger.Debug("reordered deletions before creations in delta batch")
	}

	return items
}

// partitionDeletionsFirst rewrites the given slots so deleted items come
// first, preserving the original relative order within each group. It reports
// whether anything moved.
func partitionDeletionsFirst(items []Item, slots []int) bool {
	if len(slots) < 2 {
		return false
	}

	ordered := make([]Item, 0, len(slots))
	for _, slot := range slots {
		if items[slot].IsDeleted {
			ordered = append(ordered, items[slot])
		}
	}

	if len(ordered) == 0 || len(ordered) == len(slots) {
		return false
	}

	for _, slot := range slots {
		if !items[slot].IsDeleted {
			ordered = append(ordered, items[slot])
		}
	}

	moved := false

	for i, slot := range slots {
		if items[slot].ID != ordered[i].ID {
			moved = true
		}

		items[slot] = ordered[i]
	}

	return moved
}
