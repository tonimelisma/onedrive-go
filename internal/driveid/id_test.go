package driveid

import (
	"database/sql/driver"
	"testing"
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
			if got.String() != tt.want {
				t.Errorf("New(%q) = %q, want %q", tt.raw, got.String(), tt.want)
			}
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
			if got := tt.id.IsZero(); got != tt.want {
				t.Errorf("ID{%q}.IsZero() = %v, want %v", tt.id.String(), got, tt.want)
			}
		})
	}
}

func TestID_Equal(t *testing.T) {
	a := New("ABC123DEF4567890")
	b := New("abc123def4567890")
	c := New("different1234567")

	if !a.Equal(b) {
		t.Error("expected case-different IDs to be equal after normalization")
	}

	if a.Equal(c) {
		t.Error("expected different IDs to not be equal")
	}
}

func TestID_MarshalText(t *testing.T) {
	id := New("ABC123DEF4567890")

	data, err := id.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText() error: %v", err)
	}

	if string(data) != "abc123def4567890" {
		t.Errorf("MarshalText() = %q, want %q", string(data), "abc123def4567890")
	}
}

func TestID_UnmarshalText(t *testing.T) {
	var id ID

	err := id.UnmarshalText([]byte("ABC123DEF4567890"))
	if err != nil {
		t.Fatalf("UnmarshalText() error: %v", err)
	}

	if id.String() != "abc123def4567890" {
		t.Errorf("UnmarshalText result = %q, want %q", id.String(), "abc123def4567890")
	}
}

func TestID_ScanAndValue(t *testing.T) {
	t.Run("scan string", func(t *testing.T) {
		var id ID
		if err := id.Scan("ABC123def4567890"); err != nil {
			t.Fatalf("Scan(string) error: %v", err)
		}

		if id.String() != "abc123def4567890" {
			t.Errorf("Scan(string) = %q, want %q", id.String(), "abc123def4567890")
		}
	})

	t.Run("scan bytes", func(t *testing.T) {
		var id ID
		if err := id.Scan([]byte("ABC123def4567890")); err != nil {
			t.Fatalf("Scan([]byte) error: %v", err)
		}

		if id.String() != "abc123def4567890" {
			t.Errorf("Scan([]byte) = %q, want %q", id.String(), "abc123def4567890")
		}
	})

	t.Run("scan nil produces zero ID", func(t *testing.T) {
		var id ID
		if err := id.Scan(nil); err != nil {
			t.Fatalf("Scan(nil) error: %v", err)
		}

		if !id.IsZero() {
			t.Errorf("Scan(nil) produced non-zero ID: %q", id.String())
		}
	})

	t.Run("scan unsupported type returns error", func(t *testing.T) {
		var id ID
		if err := id.Scan(42); err == nil {
			t.Error("Scan(int) should return error")
		}
	})

	t.Run("zero ID writes nil", func(t *testing.T) {
		id := ID{}

		val, err := id.Value()
		if err != nil {
			t.Fatalf("Value() error: %v", err)
		}

		if val != nil {
			t.Errorf("zero ID.Value() = %v, want nil", val)
		}
	})

	t.Run("non-zero ID writes string", func(t *testing.T) {
		id := New("abc123def4567890")

		val, err := id.Value()
		if err != nil {
			t.Fatalf("Value() error: %v", err)
		}

		s, ok := val.(string)
		if !ok {
			t.Fatalf("Value() type = %T, want string", val)
		}

		if s != "abc123def4567890" {
			t.Errorf("Value() = %q, want %q", s, "abc123def4567890")
		}
	})
}

func TestID_RoundTrip(t *testing.T) {
	// Verify Scan(Value()) round-trip preserves the ID.
	original := New("ABC123DEF4567890")

	val, err := original.Value()
	if err != nil {
		t.Fatalf("Value() error: %v", err)
	}

	var restored ID
	if err := restored.Scan(val); err != nil {
		t.Fatalf("Scan() error: %v", err)
	}

	if !original.Equal(restored) {
		t.Errorf("round-trip failed: original=%q, restored=%q", original.String(), restored.String())
	}
}

// Verify the sql.Scanner compile-time assertion works (it's checked via
// the interface assertion in id.go, but we exercise it here to be thorough).
func TestID_DriverValuer(t *testing.T) {
	var _ driver.Valuer = ID{}
}

func TestNew_EmptyEqualsZeroValue(t *testing.T) {
	// New("") must produce the same ID as the zero value ID{}.
	// This ensures a single zero representation — no identity split.
	if !New("").Equal(ID{}) {
		t.Error("New(\"\") must equal ID{}")
	}

	if !New("").IsZero() {
		t.Error("New(\"\") must be zero")
	}
}

func TestID_EmptyString_SQLRoundTrip(t *testing.T) {
	// Verify SQL roundtrip: New("") → Value() → Scan() → Equal(New("")).
	original := New("")

	val, err := original.Value()
	if err != nil {
		t.Fatalf("Value() error: %v", err)
	}

	// Zero ID writes nil to SQL.
	if val != nil {
		t.Fatalf("Value() = %v, want nil for zero ID", val)
	}

	var restored ID
	if err := restored.Scan(val); err != nil {
		t.Fatalf("Scan() error: %v", err)
	}

	if !original.Equal(restored) {
		t.Errorf("round-trip failed: original=%q, restored=%q", original.String(), restored.String())
	}
}
