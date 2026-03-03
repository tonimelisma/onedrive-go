package driveid

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewItemKey(t *testing.T) {
	driveID := New("abc123def4567890")
	key := NewItemKey(driveID, "item-001")

	assert.Equal(t, "abc123def4567890", key.DriveID.String())
	assert.Equal(t, "item-001", key.ItemID)
}

func TestItemKey_String(t *testing.T) {
	key := NewItemKey(New("abc123def4567890"), "item-001")

	want := "abc123def4567890:item-001"
	assert.Equal(t, want, key.String())
}

func TestItemKey_IsZero(t *testing.T) {
	tests := []struct {
		name string
		key  ItemKey
		want bool
	}{
		{"zero value", ItemKey{}, true},
		{"only drive set", ItemKey{DriveID: New("abc123def4567890")}, false},
		{"only item set", ItemKey{ItemID: "item-001"}, false},
		{"both set", NewItemKey(New("abc123def4567890"), "item-001"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.key.IsZero())
		})
	}
}

func TestItemKey_MapKey(t *testing.T) {
	// Verify ItemKey works as a map key (compile-time + runtime check).
	key1 := NewItemKey(New("abc123def4567890"), "item-001")
	key2 := NewItemKey(New("ABC123DEF4567890"), "item-001") // same after normalization
	key3 := NewItemKey(New("abc123def4567890"), "item-002") // different item

	m := map[ItemKey]string{
		key1: "first",
	}

	// key2 should match key1 because the drive ID normalizes identically.
	v, ok := m[key2]
	assert.True(t, ok, "expected key2 to find the same entry as key1")
	assert.Equal(t, "first", v)

	// key3 has a different item ID, should not match.
	_, ok = m[key3]
	assert.False(t, ok, "expected key3 to not match key1")
}

func TestItemKey_Equality(t *testing.T) {
	a := NewItemKey(New("abc123def4567890"), "item-001")
	b := NewItemKey(New("ABC123DEF4567890"), "item-001")
	c := NewItemKey(New("abc123def4567890"), "item-002")

	assert.Equal(t, a, b, "expected a == b after normalization")
	assert.NotEqual(t, a, c, "expected a != c with different item IDs")
}
