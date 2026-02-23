package driveid

import (
	"testing"
)

func TestNewItemKey(t *testing.T) {
	driveID := New("abc123def4567890")
	key := NewItemKey(driveID, "item-001")

	if key.DriveID.String() != "abc123def4567890" {
		t.Errorf("DriveID = %q, want %q", key.DriveID.String(), "abc123def4567890")
	}

	if key.ItemID != "item-001" {
		t.Errorf("ItemID = %q, want %q", key.ItemID, "item-001")
	}
}

func TestItemKey_String(t *testing.T) {
	key := NewItemKey(New("abc123def4567890"), "item-001")

	want := "abc123def4567890:item-001"
	if got := key.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
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
			if got := tt.key.IsZero(); got != tt.want {
				t.Errorf("IsZero() = %v, want %v", got, tt.want)
			}
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
	if v, ok := m[key2]; !ok || v != "first" {
		t.Error("expected key2 to find the same entry as key1")
	}

	// key3 has a different item ID, should not match.
	if _, ok := m[key3]; ok {
		t.Error("expected key3 to not match key1")
	}
}

func TestItemKey_Equality(t *testing.T) {
	a := NewItemKey(New("abc123def4567890"), "item-001")
	b := NewItemKey(New("ABC123DEF4567890"), "item-001")
	c := NewItemKey(New("abc123def4567890"), "item-002")

	if a != b {
		t.Error("expected a == b after normalization")
	}

	if a == c {
		t.Error("expected a != c with different item IDs")
	}
}
