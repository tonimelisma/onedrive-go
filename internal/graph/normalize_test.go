package graph

import (
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertNormalizedItems(
	t *testing.T,
	items []Item,
	want []Item,
	normalize func([]Item, *slog.Logger) []Item,
) {
	t.Helper()
	assert.Equal(t, want, normalize(items, testNoopLogger()))
}

// Validates: R-6.7.8
func TestNormalize_NamesArePassedThroughVerbatim(t *testing.T) {
	// Live probe (Personal, 2026-10-25): create, GET, /children and /delta all
	// return these names literally. Decoding them renamed user files
	// (`a%20b.txt` became `a b.txt`) and invented subfolders (`x%2Fy`).
	names := []string{
		"a%20b.txt",
		"100%25.txt",
		"test%46.txt",
		"x%2Fy",
		"%%",
		"100%",
		"%E6%97%A5%E6%9C%AC%E8%AA%9E.txt",
		"plus+sign.txt",
		"bad%zzname.txt",
	}

	items := make([]Item, 0, len(names))
	for i, name := range names {
		items = append(items, Item{ID: fmt.Sprintf("item-%d", i), Name: name, ParentID: "root"})
	}

	result := normalizeDeltaItems(items, testNoopLogger())

	require.Len(t, result, len(names))
	for i, name := range names {
		assert.Equal(t, name, result[i].Name)
	}
}

func TestClearDeletedHashes(t *testing.T) {
	now := time.Now().UTC()
	items := []Item{
		{
			ID:           "deleted-1",
			Name:         "old.txt",
			IsDeleted:    true,
			QuickXorHash: "aGFzaA==",
			SHA1Hash:     "abc123",
			SHA256Hash:   "def456",
			ModifiedAt:   now,
		},
		{
			ID:           "alive-1",
			Name:         "current.txt",
			IsDeleted:    false,
			QuickXorHash: "bGl2ZQ==",
			SHA1Hash:     "789xyz",
			SHA256Hash:   "ghi012",
			ModifiedAt:   now,
		},
	}

	result := clearDeletedHashes(items, testNoopLogger())

	assert.Empty(t, result[0].QuickXorHash)
	assert.Empty(t, result[0].SHA1Hash)
	assert.Empty(t, result[0].SHA256Hash)

	assert.Equal(t, "bGl2ZQ==", result[1].QuickXorHash)
	assert.Equal(t, "789xyz", result[1].SHA1Hash)
	assert.Equal(t, "ghi012", result[1].SHA256Hash)
}

func TestClearDeletedHashes_DeletedWithNoHashes(t *testing.T) {
	items := []Item{
		{ID: "deleted-nohash", Name: "empty.txt", IsDeleted: true},
	}

	result := clearDeletedHashes(items, testNoopLogger())

	assert.Empty(t, result[0].QuickXorHash)
	assert.Empty(t, result[0].SHA1Hash)
	assert.Empty(t, result[0].SHA256Hash)
}

// Validates: R-6.7.10
func TestDeduplicateItems(t *testing.T) {
	items := []Item{
		{ID: "item-1", Name: "v1"},
		{ID: "item-1", Name: "v2"},
		{ID: "item-1", Name: "v3"},
		{ID: "item-2", Name: "other"},
	}

	assertNormalizedItems(t, items, []Item{
		{ID: "item-1", Name: "v3"},
		{ID: "item-2", Name: "other"},
	}, deduplicateItems)
}

func TestDeduplicateItems_NoDuplicates(t *testing.T) {
	items := []Item{
		{ID: "item-1", Name: "first"},
		{ID: "item-2", Name: "second"},
		{ID: "item-3", Name: "third"},
	}

	result := deduplicateItems(items, testNoopLogger())

	assert.Len(t, result, 3)
	assert.Equal(t, "item-1", result[0].ID)
	assert.Equal(t, "item-2", result[1].ID)
	assert.Equal(t, "item-3", result[2].ID)
}

func TestDeduplicateItems_Empty(t *testing.T) {
	result := deduplicateItems([]Item{}, testNoopLogger())
	assert.Empty(t, result)
}

func TestReorderDeletions(t *testing.T) {
	items := []Item{
		{ID: "create-1", Name: "new.txt", ParentID: "parent-a", IsDeleted: false},
		{ID: "delete-1", Name: "old.txt", ParentID: "parent-a", IsDeleted: true},
	}

	result := reorderDeletions(items, testNoopLogger())

	assert.Len(t, result, 2)
	assert.True(t, result[0].IsDeleted)
	assert.Equal(t, "delete-1", result[0].ID)
	assert.False(t, result[1].IsDeleted)
	assert.Equal(t, "create-1", result[1].ID)
}

func TestReorderDeletions_DifferentParents(t *testing.T) {
	items := []Item{
		{ID: "create-1", Name: "new.txt", ParentID: "parent-a", IsDeleted: false},
		{ID: "delete-1", Name: "old.txt", ParentID: "parent-b", IsDeleted: true},
	}

	result := reorderDeletions(items, testNoopLogger())

	assert.Len(t, result, 2)
	assert.Equal(t, "create-1", result[0].ID)
	assert.Equal(t, "delete-1", result[1].ID)
}

func TestReorderDeletions_MultipleParents(t *testing.T) {
	items := []Item{
		{ID: "create-a1", Name: "new-a.txt", ParentID: "parent-a", IsDeleted: false},
		{ID: "delete-a1", Name: "old-a.txt", ParentID: "parent-a", IsDeleted: true},
		{ID: "create-b1", Name: "new-b.txt", ParentID: "parent-b", IsDeleted: false},
		{ID: "delete-b1", Name: "old-b.txt", ParentID: "parent-b", IsDeleted: true},
	}

	result := reorderDeletions(items, testNoopLogger())

	assert.Len(t, result, 4)

	parentAItems := filterByParent(result, "parent-a")
	assert.True(t, parentAItems[0].IsDeleted)
	assert.False(t, parentAItems[1].IsDeleted)

	parentBItems := filterByParent(result, "parent-b")
	assert.True(t, parentBItems[0].IsDeleted)
	assert.False(t, parentBItems[1].IsDeleted)
}

func TestReorderDeletions_Empty(t *testing.T) {
	result := reorderDeletions([]Item{}, testNoopLogger())
	assert.Empty(t, result)
}

// Validates: R-6.7.8, R-6.7.9, R-6.7.10
func TestNormalizeDeltaItems_FullPipeline(t *testing.T) {
	items := []Item{
		{ID: "pkg-1", Name: "Notebook.one", IsPackage: true, ParentID: "root"},
		{ID: "deleted-1", Name: "old.txt", IsDeleted: true, QuickXorHash: "bogus", ParentID: "folder-a"},
		{ID: "dup-1", Name: "v1-of-file", ParentID: "folder-a"},
		{ID: "create-1", Name: "new%20file.txt", ParentID: "folder-a", IsDeleted: false},
		{ID: "dup-1", Name: "v2-of-file", ParentID: "folder-a"},
	}

	result := normalizeDeltaItems(items, testNoopLogger())

	assert.Len(t, result, 4)

	ids := make(map[string]Item, len(result))
	for i := range result {
		ids[result[i].ID] = result[i]
	}
	assert.True(t, ids["deleted-1"].IsDeleted)
	assert.Empty(t, ids["deleted-1"].QuickXorHash)
	assert.True(t, ids["pkg-1"].IsPackage)

	allIDs := make([]string, 0, len(result))
	for i := range result {
		allIDs = append(allIDs, result[i].ID)
	}
	assert.Contains(t, allIDs, "create-1")
	assert.Contains(t, allIDs, "dup-1")

	for _, item := range result {
		if item.ID == "dup-1" {
			assert.Equal(t, "v2-of-file", item.Name)
		}

		// Names are opaque: the percent sequence is part of the name.
		if item.ID == "create-1" {
			assert.Equal(t, "new%20file.txt", item.Name)
		}
	}
}

// filterByParent is a test helper that returns items matching the given parentID.
func filterByParent(items []Item, parentID string) []Item {
	var result []Item

	for i := range items {
		if items[i].ParentID == parentID {
			result = append(result, items[i])
		}
	}

	return result
}
