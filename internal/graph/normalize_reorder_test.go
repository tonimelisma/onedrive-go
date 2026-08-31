package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func reorderTestItemIDs(items []Item) []string {
	ids := make([]string, 0, len(items))
	for i := range items {
		ids = append(ids, items[i].ID)
	}

	return ids
}

// Validates: R-6.7.28
//
// A delta batch spans folders, so items sharing a parent are usually not
// adjacent. Expressing this as a sort needed a comparator that calls items
// with different parents equal, which is not a valid ordering: a delete and a
// create at parent P are ordered, yet each compares equal to an unrelated item
// at parent Q. A stable sort given that comparator quietly did nothing
// whenever another parent's item sat between the two.
func TestReorderDeletions_MovesDeletionAcrossAnInterleavedParent(t *testing.T) {
	t.Parallel()

	items := []Item{
		{ID: "create", ParentID: "P"},
		{ID: "other", ParentID: "Q"},
		{ID: "delete", ParentID: "P", IsDeleted: true},
	}

	got := reorderDeletions(items, testNoopLogger())

	require.Len(t, got, 3)
	assert.Equal(t, []string{"delete", "other", "create"}, reorderTestItemIDs(got),
		"the deletion must precede the creation at its own parent, and the unrelated item keeps its slot")
}

// Validates: R-6.7.28
func TestReorderDeletions_MovesDeletionAcrossSeveralInterleavedParents(t *testing.T) {
	t.Parallel()

	items := []Item{
		{ID: "c1", ParentID: "P"},
		{ID: "q1", ParentID: "Q"},
		{ID: "r1", ParentID: "R"},
		{ID: "d1", ParentID: "P", IsDeleted: true},
	}

	got := reorderDeletions(items, testNoopLogger())

	assert.Equal(t, []string{"d1", "q1", "r1", "c1"}, reorderTestItemIDs(got))
}

// Validates: R-6.7.28
//
// Items belonging to other parents must not be disturbed, which is what makes
// a per-parent partition the right shape rather than a global sort.
func TestReorderDeletions_LeavesOtherParentsOrderUntouched(t *testing.T) {
	t.Parallel()

	items := []Item{
		{ID: "q1", ParentID: "Q"},
		{ID: "q2", ParentID: "Q", IsDeleted: true},
		{ID: "r1", ParentID: "R"},
		{ID: "r2", ParentID: "R"},
	}

	got := reorderDeletions(items, testNoopLogger())

	assert.Equal(t, []string{"q2", "q1", "r1", "r2"}, reorderTestItemIDs(got),
		"R keeps its original order; only Q partitions")
}

// Validates: R-6.7.28
func TestReorderDeletions_AdjacentPairStillWorks(t *testing.T) {
	t.Parallel()

	items := []Item{
		{ID: "create", ParentID: "P"},
		{ID: "delete", ParentID: "P", IsDeleted: true},
	}

	assert.Equal(t, []string{"delete", "create"}, reorderTestItemIDs(reorderDeletions(items, testNoopLogger())))
}
