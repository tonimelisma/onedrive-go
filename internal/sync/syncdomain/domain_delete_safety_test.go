package syncdomain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A confirmation names a held set whatever the order of its deletes, and any
// change to the set (one more delete, another item at a path) names another
// set. Its written form reads back as itself.
//
// Validates: R-6.2.17
func TestConfirmationOf_NamesExactlyTheHeldSet(t *testing.T) {
	t.Parallel()

	a := HeldDelete{Path: "Docs/a.txt", Side: DeleteSafetySideRemote, Reason: DeleteSafetyReasonTooMany, ItemType: ItemTypeFile, ItemID: "item-a"}
	b := HeldDelete{Path: "Docs/b.txt", Side: DeleteSafetySideRemote, Reason: DeleteSafetyReasonTooMany, ItemType: ItemTypeFile, ItemID: "item-b"}
	held := []HeldDelete{a, b}

	confirmation := ConfirmationOf(held)
	assert.Equal(t, 2, confirmation.Count)
	assert.Len(t, confirmation.Token, heldDeleteTokenLength)
	assert.Equal(t, confirmation, ConfirmationOf([]HeldDelete{b, a}), "the order of the deletes does not matter")
	assert.True(t, confirmation.Matches(held))

	assert.False(t, confirmation.Matches([]HeldDelete{a}), "a set with a delete less is another set")
	replaced := b
	replaced.ItemID = "item-other"
	assert.False(t, confirmation.Matches([]HeldDelete{a, replaced}), "another item at the same path is another set")

	parsed, err := ParseHeldDeleteConfirmation(confirmation.String())
	require.NoError(t, err)
	assert.Equal(t, confirmation, parsed)

	for _, malformed := range []string{"", "2", "0-0123456789abcdef", "x-0123456789abcdef", "2-0123", "2-zzzzzzzzzzzzzzzz"} {
		_, err := ParseHeldDeleteConfirmation(malformed)
		assert.Error(t, err, malformed)
	}
}
