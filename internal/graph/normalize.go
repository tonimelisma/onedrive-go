package graph

import (
	"log/slog"
	"net/url"
	"slices"
)

// normalizeDeltaItems applies delta-specific quirk handling to a batch of items.
// This handles quirks that only manifest in delta responses, not in single-item
// or list-children responses. The pipeline runs in a fixed order:
// 0. URL-decode item names (Graph API sometimes returns %20-encoded names)
// 1. Filter out OneNote packages (not syncable)
// 2. Clear bogus hashes on deleted items
// 3. Deduplicate items that appear multiple times (keep last occurrence)
// 4. Reorder so deletions at a parent are processed before creations
func normalizeDeltaItems(items []Item, logger *slog.Logger) []Item {
	items = decodeURLEncodedNames(items, logger)
	items = filterPackages(items, logger)
	items = clearDeletedHashes(items, logger)
	items = deduplicateItems(items, logger)
	items = reorderDeletions(items, logger)

	return items
}

// filterPackages removes items where IsPackage is true. OneNote packages
// should be skipped entirely during sync — they are compound objects that
// cannot be meaningfully synced as files.
func filterPackages(items []Item, logger *slog.Logger) []Item {
	result := make([]Item, 0, len(items))

	for i := range items {
		if items[i].IsPackage {
			logger.Debug("filtering out package item",
				slog.String("item_id", items[i].ID),
				slog.String("name", items[i].Name),
			)

			continue
		}

		result = append(result, items[i])
	}

	if filtered := len(items) - len(result); filtered > 0 {
		logger.Info("filtered package items from delta batch",
			slog.Int("filtered_count", filtered),
			slog.Int("remaining_count", len(result)),
		)
	}

	return result
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

// reorderDeletions sorts items so that deletions come before non-deletions
// when they share the same ParentID. This prevents "item already exists"
// errors when processing a rename-then-recreate at the same parent.
// Uses stable sort to preserve relative order of items with different parents.
func reorderDeletions(items []Item, logger *slog.Logger) []Item {
	if len(items) == 0 {
		return items
	}

	reordered := false

	slices.SortStableFunc(items, func(a, b Item) int {
		// Only reorder items that share a parent.
		if a.ParentID != b.ParentID {
			return 0
		}

		// Deletions sort before non-deletions at the same parent.
		switch {
		case a.IsDeleted && !b.IsDeleted:
			reordered = true
			return -1
		case !a.IsDeleted && b.IsDeleted:
			reordered = true
			return 1
		default:
			return 0
		}
	})

	if reordered {
		logger.Debug("reordered deletions before creations in delta batch")
	}

	return items
}

// decodeURLEncodedNames applies url.PathUnescape to item names.
// The Graph API sometimes returns URL-encoded names (e.g., "my%20file.txt")
// in delta responses, particularly for items in shared folders on Personal
// accounts. This step normalizes names before further processing.
func decodeURLEncodedNames(items []Item, logger *slog.Logger) []Item {
	decoded := 0

	for i := range items {
		unescaped, err := url.PathUnescape(items[i].Name)
		if err != nil {
			// If unescaping fails, keep the original name — this should be
			// extremely rare (malformed percent-encoding from the API).
			logger.Debug("failed to URL-decode item name, keeping original",
				slog.String("item_id", items[i].ID),
				slog.String("name", items[i].Name),
				slog.String("error", err.Error()),
			)

			continue
		}

		if unescaped != items[i].Name {
			logger.Debug("URL-decoded item name",
				slog.String("item_id", items[i].ID),
				slog.String("encoded", items[i].Name),
				slog.String("decoded", unescaped),
			)

			items[i].Name = unescaped
			decoded++
		}
	}

	if decoded > 0 {
		logger.Info("URL-decoded item names in delta batch",
			slog.Int("decoded_count", decoded),
		)
	}

	return items
}
