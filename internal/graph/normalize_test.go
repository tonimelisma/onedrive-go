package graph

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFilterPackages(t *testing.T) {
	items := []Item{
		{ID: "file-1", Name: "doc.txt", IsPackage: false},
		{ID: "pkg-1", Name: "Notebook.one", IsPackage: true},
		{ID: "file-2", Name: "photo.jpg", IsPackage: false},
		{ID: "pkg-2", Name: "Notebook2.one", IsPackage: true},
	}

	result := filterPackages(items, testNoopLogger())

	assert.Len(t, result, 2)
	assert.Equal(t, "file-1", result[0].ID)
	assert.Equal(t, "file-2", result[1].ID)
}

func TestFilterPackages_AllPackages(t *testing.T) {
	items := []Item{
		{ID: "pkg-1", Name: "Notebook1.one", IsPackage: true},
		{ID: "pkg-2", Name: "Notebook2.one", IsPackage: true},
	}

	result := filterPackages(items, testNoopLogger())

	assert.Empty(t, result)
}

func TestFilterPackages_NoPackages(t *testing.T) {
	items := []Item{
		{ID: "file-1", Name: "doc.txt"},
		{ID: "file-2", Name: "photo.jpg"},
	}

	result := filterPackages(items, testNoopLogger())

	assert.Len(t, result, 2)
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

	assert.Equal(t, "", result[0].QuickXorHash)
	assert.Equal(t, "", result[0].SHA1Hash)
	assert.Equal(t, "", result[0].SHA256Hash)

	assert.Equal(t, "bGl2ZQ==", result[1].QuickXorHash)
	assert.Equal(t, "789xyz", result[1].SHA1Hash)
	assert.Equal(t, "ghi012", result[1].SHA256Hash)
}

func TestClearDeletedHashes_DeletedWithNoHashes(t *testing.T) {
	items := []Item{
		{ID: "deleted-nohash", Name: "empty.txt", IsDeleted: true},
	}

	result := clearDeletedHashes(items, testNoopLogger())

	assert.Equal(t, "", result[0].QuickXorHash)
	assert.Equal(t, "", result[0].SHA1Hash)
	assert.Equal(t, "", result[0].SHA256Hash)
}

func TestDeduplicateItems(t *testing.T) {
	items := []Item{
		{ID: "item-1", Name: "v1"},
		{ID: "item-1", Name: "v2"},
		{ID: "item-1", Name: "v3"},
		{ID: "item-2", Name: "other"},
	}

	result := deduplicateItems(items, testNoopLogger())

	assert.Len(t, result, 2)
	assert.Equal(t, "item-1", result[0].ID)
	assert.Equal(t, "v3", result[0].Name)
	assert.Equal(t, "item-2", result[1].ID)
	assert.Equal(t, "other", result[1].Name)
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

func TestNormalizeDeltaItems_FullPipeline(t *testing.T) {
	items := []Item{
		{ID: "pkg-1", Name: "Notebook.one", IsPackage: true, ParentID: "root"},
		{ID: "deleted-1", Name: "old.txt", IsDeleted: true, QuickXorHash: "bogus", ParentID: "folder-a"},
		{ID: "dup-1", Name: "v1-of-file", ParentID: "folder-a"},
		{ID: "create-1", Name: "new.txt", ParentID: "folder-a", IsDeleted: false},
		{ID: "dup-1", Name: "v2-of-file", ParentID: "folder-a"},
	}

	result := normalizeDeltaItems(items, testNoopLogger())

	assert.Len(t, result, 3)

	assert.Equal(t, "deleted-1", result[0].ID)
	assert.True(t, result[0].IsDeleted)
	assert.Equal(t, "", result[0].QuickXorHash)

	assert.False(t, result[1].IsDeleted)
	assert.False(t, result[2].IsDeleted)

	ids := []string{result[1].ID, result[2].ID}
	assert.Contains(t, ids, "create-1")
	assert.Contains(t, ids, "dup-1")

	for _, item := range result {
		if item.ID == "dup-1" {
			assert.Equal(t, "v2-of-file", item.Name)
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
