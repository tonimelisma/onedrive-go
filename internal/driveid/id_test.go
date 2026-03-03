package driveid

import (
	"database/sql/driver"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "empty string produces zero ID",
			raw:  "",
			want: "",
		},
		{
			name: "15-char personal ID gets zero-padded",
			raw:  "abc123def456789",
			want: "0abc123def456789",
		},
		{
			name: "16-char ID unchanged (already minimum length)",
			raw:  "abc123def4567890",
			want: "abc123def4567890",
		},
		{
			name: "uppercase lowercased",
			raw:  "ABC123DEF4567890",
			want: "abc123def4567890",
		},
		{
			name: "business ID with b! prefix lowercased, no padding",
			raw:  "b!SomeLongBase64",
			want: "b!somelongbase64",
		},
		{
			name: "long business ID stays as-is except lowercase",
			raw:  "b!Q2xF_a9S0k-2mDjR4cZOvH_ABCDEFGHIJKLMNOP",
			want: "b!q2xf_a9s0k-2mdjr4czovh_abcdefghijklmnop",
		},
		{
			name: "short 3-char ID padded to 16",
			raw:  "abc",
			want: "0000000000000abc",
		},
		{
			name: "idempotent - already normalized",
			raw:  "0abc123def456789",
			want: "0abc123def456789",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := New(tt.raw)
			assert.Equal(t, tt.want, got.String())
		})
	}
}

func TestID_IsZero(t *testing.T) {
	tests := []struct {
		name string
		id   ID
		want bool
	}{
		{"zero value struct", ID{}, true},
		{"empty string via New", New(""), true},
		{"non-zero ID", New("abc123def4567890"), false},
		{"padded but non-zero", New("abc"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.id.IsZero())
		})
	}
}

func TestID_Equal(t *testing.T) {
	a := New("ABC123DEF4567890")
	b := New("abc123def4567890")
	c := New("different1234567")

	assert.True(t, a.Equal(b), "expected case-different IDs to be equal after normalization")
	assert.False(t, a.Equal(c), "expected different IDs to not be equal")
}

func TestID_MarshalText(t *testing.T) {
	id := New("ABC123DEF4567890")

	data, err := id.MarshalText()
	require.NoError(t, err)
	assert.Equal(t, "abc123def4567890", string(data))
}

func TestID_UnmarshalText(t *testing.T) {
	var id ID

	err := id.UnmarshalText([]byte("ABC123DEF4567890"))
	require.NoError(t, err)
	assert.Equal(t, "abc123def4567890", id.String())
}

func TestID_ScanAndValue(t *testing.T) {
	t.Run("scan string", func(t *testing.T) {
		var id ID
		err := id.Scan("ABC123def4567890")
		require.NoError(t, err)
		assert.Equal(t, "abc123def4567890", id.String())
	})

	t.Run("scan bytes", func(t *testing.T) {
		var id ID
		err := id.Scan([]byte("ABC123def4567890"))
		require.NoError(t, err)
		assert.Equal(t, "abc123def4567890", id.String())
	})

	t.Run("scan nil produces zero ID", func(t *testing.T) {
		var id ID
		err := id.Scan(nil)
		require.NoError(t, err)
		assert.True(t, id.IsZero())
	})

	t.Run("scan unsupported type returns error", func(t *testing.T) {
		var id ID
		err := id.Scan(42)
		assert.Error(t, err)
	})

	t.Run("zero ID writes nil", func(t *testing.T) {
		id := ID{}

		val, err := id.Value()
		require.NoError(t, err)
		assert.Nil(t, val)
	})

	t.Run("non-zero ID writes string", func(t *testing.T) {
		id := New("abc123def4567890")

		val, err := id.Value()
		require.NoError(t, err)

		s, ok := val.(string)
		require.True(t, ok, "Value() type = %T, want string", val)
		assert.Equal(t, "abc123def4567890", s)
	})
}

func TestID_RoundTrip(t *testing.T) {
	// Verify Scan(Value()) round-trip preserves the ID.
	original := New("ABC123DEF4567890")

	val, err := original.Value()
	require.NoError(t, err)

	var restored ID
	err = restored.Scan(val)
	require.NoError(t, err)

	assert.True(t, original.Equal(restored), "round-trip failed: original=%q, restored=%q", original.String(), restored.String())
}

// Verify the sql.Scanner compile-time assertion works (it's checked via
// the interface assertion in id.go, but we exercise it here to be thorough).
func TestID_DriverValuer(t *testing.T) {
	var _ driver.Valuer = ID{}
}

func TestNew_EmptyEqualsZeroValue(t *testing.T) {
	// New("") must produce the same ID as the zero value ID{}.
	// This ensures a single zero representation — no identity split.
	assert.True(t, New("").Equal(ID{}), "New(\"\") must equal ID{}")
	assert.True(t, New("").IsZero(), "New(\"\") must be zero")
}

func TestID_EmptyString_SQLRoundTrip(t *testing.T) {
	// Verify SQL roundtrip: New("") → Value() → Scan() → Equal(New("")).
	original := New("")

	val, err := original.Value()
	require.NoError(t, err)

	// Zero ID writes nil to SQL.
	require.Nil(t, val, "Value() should be nil for zero ID")

	var restored ID
	err = restored.Scan(val)
	require.NoError(t, err)

	assert.True(t, original.Equal(restored), "round-trip failed: original=%q, restored=%q", original.String(), restored.String())
}
