package driveid

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestNew_TruncatedDriveIDZeroPadding validates that Personal accounts
// returning short (<16 char) drive IDs are zero-padded consistently.
// Graph API bug: Personal accounts sometimes drop the leading zero.
func TestNew_TruncatedDriveIDZeroPadding(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "15 char personal ID padded to 16",
			raw:  "24470056f5c3e43",
			want: "024470056f5c3e43",
		},
		{
			name: "14 char ID padded to 16",
			raw:  "4470056f5c3e43",
			want: "004470056f5c3e43",
		},
		{
			name: "single char padded to 16",
			raw:  "a",
			want: "000000000000000a",
		},
		{
			name: "exactly 16 chars no padding",
			raw:  "024470056f5c3e43",
			want: "024470056f5c3e43",
		},
		{
			name: "17 chars no padding",
			raw:  "0024470056f5c3e43",
			want: "0024470056f5c3e43",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := New(tt.raw)
			assert.Equal(t, tt.want, got.String())
		})
	}
}

// TestNew_CaseNormalization validates that drive IDs are lowercased
// regardless of input casing. Graph API returns inconsistent casing
// across endpoints (documented in tier1-research).
func TestNew_CaseNormalization(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "all uppercase",
			raw:  "D8EBBD13A0551216",
			want: "d8ebbd13a0551216",
		},
		{
			name: "mixed case",
			raw:  "D8eBbD13a0551216",
			want: "d8ebbd13a0551216",
		},
		{
			name: "business ID with mixed case",
			raw:  "b!Q2xFa9S0kUaEz71IGb4HDhDwHj",
			want: "b!q2xfa9s0kuaez71igb4hdhdwhj",
		},
		{
			name: "uppercase short ID gets padded and lowered",
			raw:  "ABC",
			want: "0000000000000abc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := New(tt.raw)
			assert.Equal(t, tt.want, got.String())
		})
	}
}

// TestNew_Idempotent verifies that normalizing an already-normalized ID
// produces the same result. This is critical for map key stability.
func TestNew_Idempotent(t *testing.T) {
	raw := "24470056f5c3e43"
	first := New(raw)
	second := New(first.String())
	assert.Equal(t, first, second)
	assert.Equal(t, first.String(), second.String())
}

// TestID_Equal_CrossCaseMatch verifies that IDs differing only in
// API casing are equal after normalization.
func TestID_Equal_CrossCaseMatch(t *testing.T) {
	upper := New("D8EBBD13A0551216")
	lower := New("d8ebbd13a0551216")
	assert.True(t, upper.Equal(lower))
}

// TestID_Equal_PaddedMatch verifies that a truncated ID and its
// zero-padded form are equal after normalization.
func TestID_Equal_PaddedMatch(t *testing.T) {
	short := New("24470056f5c3e43") // 15 chars
	full := New("024470056f5c3e43") // 16 chars
	assert.True(t, short.Equal(full))
}

// TestID_IsZero_AllZerosPadded verifies that zero-padded all-zeros
// is treated as zero value.
func TestID_IsZero_AllZerosPadded(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		zero bool
	}{
		{"empty string", "", true},
		{"all zeros 16 chars", "0000000000000000", true},
		{"short zeros padded to 16", "0", true},
		{"single non-zero", "1", false},
		{"non-zero padded", "0000000000000001", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := New(tt.raw)
			assert.Equal(t, tt.zero, id.IsZero())
		})
	}
}

// TestItemKey_NormalizedEquality verifies that ItemKeys with differently
// cased or padded DriveIDs but same logical identity are equal when used
// as map keys.
func TestItemKey_NormalizedEquality(t *testing.T) {
	key1 := NewItemKey(New("D8EBBD13A0551216"), "item-123")
	key2 := NewItemKey(New("d8ebbd13a0551216"), "item-123")
	assert.Equal(t, key1, key2)

	// Also verify as map key.
	m := map[ItemKey]string{key1: "value"}
	assert.Equal(t, "value", m[key2])
}

// TestItemKey_PaddedDriveIDEquality verifies that ItemKeys with short
// vs full DriveIDs are equal.
func TestItemKey_PaddedDriveIDEquality(t *testing.T) {
	key1 := NewItemKey(New("24470056f5c3e43"), "item-abc")
	key2 := NewItemKey(New("024470056f5c3e43"), "item-abc")
	assert.Equal(t, key1, key2)
}

// TestItemKey_DifferentItemIDNotEqual verifies that same DriveID but
// different ItemID produces different keys.
func TestItemKey_DifferentItemIDNotEqual(t *testing.T) {
	key1 := NewItemKey(New("d8ebbd13a0551216"), "item-1")
	key2 := NewItemKey(New("d8ebbd13a0551216"), "item-2")
	assert.NotEqual(t, key1, key2)
}

// TestCanonicalID_SharePointTokenMapping validates that SharePoint
// canonical IDs map to business for token lookup.
func TestCanonicalID_SharePointTokenMapping(t *testing.T) {
	sp := MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs")
	assert.True(t, sp.IsSharePoint())
	assert.Equal(t, "alice@contoso.com", sp.Email())

	tokenID := sp.TokenCanonicalID()
	assert.Equal(t, "business", tokenID.DriveType())
	assert.Equal(t, "alice@contoso.com", tokenID.Email())
	assert.False(t, tokenID.IsSharePoint())
}

// TestCanonicalID_PersonalTokenMapping verifies that personal drives
// return themselves as the token canonical ID.
func TestCanonicalID_PersonalTokenMapping(t *testing.T) {
	personal := MustCanonicalID("personal:user@example.com")
	tokenID := personal.TokenCanonicalID()
	assert.Equal(t, personal, tokenID)
}
