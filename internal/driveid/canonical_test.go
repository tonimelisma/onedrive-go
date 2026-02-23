package driveid

import (
	"testing"
)

func TestNewCanonicalID(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{
			name: "personal account",
			raw:  "personal:user@example.com",
			want: "personal:user@example.com",
		},
		{
			name: "business account",
			raw:  "business:alice@contoso.com",
			want: "business:alice@contoso.com",
		},
		{
			name: "sharepoint with site and library",
			raw:  "sharepoint:alice@contoso.com:marketing:Docs",
			want: "sharepoint:alice@contoso.com:marketing:Docs",
		},
		{
			name:    "no colon separator",
			raw:     "personal",
			wantErr: true,
		},
		{
			name:    "empty email",
			raw:     "personal:",
			wantErr: true,
		},
		{
			name:    "unknown type",
			raw:     "onprem:user@example.com",
			wantErr: true,
		},
		{
			name:    "empty string",
			raw:     "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cid, err := NewCanonicalID(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if cid.String() != tt.want {
				t.Errorf("NewCanonicalID(%q) = %q, want %q", tt.raw, cid.String(), tt.want)
			}
		})
	}
}

func TestMustCanonicalID(t *testing.T) {
	t.Run("valid input", func(t *testing.T) {
		cid := MustCanonicalID("personal:user@example.com")
		if cid.String() != "personal:user@example.com" {
			t.Errorf("MustCanonicalID() = %q, want %q", cid.String(), "personal:user@example.com")
		}
	})

	t.Run("invalid input panics", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic for invalid canonical ID")
			}
		}()

		MustCanonicalID("invalid")
	})
}

func TestConstruct(t *testing.T) {
	tests := []struct {
		name      string
		driveType string
		email     string
		want      string
		wantErr   bool
	}{
		{
			name:      "personal",
			driveType: "personal",
			email:     "user@example.com",
			want:      "personal:user@example.com",
		},
		{
			name:      "business",
			driveType: "business",
			email:     "alice@contoso.com",
			want:      "business:alice@contoso.com",
		},
		{
			name:      "unknown type",
			driveType: "onprem",
			email:     "user@example.com",
			wantErr:   true,
		},
		{
			name:      "empty email",
			driveType: "personal",
			email:     "",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cid, err := Construct(tt.driveType, tt.email)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if cid.String() != tt.want {
				t.Errorf("Construct(%q, %q) = %q, want %q", tt.driveType, tt.email, cid.String(), tt.want)
			}
		})
	}
}

func TestCanonicalID_DriveType(t *testing.T) {
	tests := []struct {
		name string
		cid  CanonicalID
		want string
	}{
		{"personal", MustCanonicalID("personal:user@example.com"), "personal"},
		{"business", MustCanonicalID("business:alice@contoso.com"), "business"},
		{"sharepoint", MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"), "sharepoint"},
		{"zero value", CanonicalID{}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cid.DriveType(); got != tt.want {
				t.Errorf("DriveType() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCanonicalID_Email(t *testing.T) {
	tests := []struct {
		name string
		cid  CanonicalID
		want string
	}{
		{"personal", MustCanonicalID("personal:user@example.com"), "user@example.com"},
		{"business", MustCanonicalID("business:alice@contoso.com"), "alice@contoso.com"},
		{
			"sharepoint extracts email cleanly",
			MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"),
			"alice@contoso.com",
		},
		{"zero value", CanonicalID{}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cid.Email(); got != tt.want {
				t.Errorf("Email() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCanonicalID_IsSharePoint(t *testing.T) {
	tests := []struct {
		name string
		cid  CanonicalID
		want bool
	}{
		{"personal is not sharepoint", MustCanonicalID("personal:user@example.com"), false},
		{"business is not sharepoint", MustCanonicalID("business:alice@contoso.com"), false},
		{"sharepoint", MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cid.IsSharePoint(); got != tt.want {
				t.Errorf("IsSharePoint() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCanonicalID_TokenCanonicalID(t *testing.T) {
	tests := []struct {
		name string
		cid  CanonicalID
		want string
	}{
		{"personal returns self", MustCanonicalID("personal:user@example.com"), "personal:user@example.com"},
		{"business returns self", MustCanonicalID("business:alice@contoso.com"), "business:alice@contoso.com"},
		{
			"sharepoint returns business",
			MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"),
			"business:alice@contoso.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cid.TokenCanonicalID()
			if got.String() != tt.want {
				t.Errorf("TokenCanonicalID() = %q, want %q", got.String(), tt.want)
			}
		})
	}
}

func TestCanonicalID_IsZero(t *testing.T) {
	zero := CanonicalID{}
	if !zero.IsZero() {
		t.Error("zero-value CanonicalID should be zero")
	}

	if MustCanonicalID("personal:user@example.com").IsZero() {
		t.Error("non-zero CanonicalID should not be zero")
	}
}

func TestCanonicalID_MarshalText(t *testing.T) {
	cid := MustCanonicalID("personal:user@example.com")

	data, err := cid.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText() error: %v", err)
	}

	if string(data) != "personal:user@example.com" {
		t.Errorf("MarshalText() = %q, want %q", string(data), "personal:user@example.com")
	}
}

func TestCanonicalID_MarshalText_Zero(t *testing.T) {
	var cid CanonicalID

	data, err := cid.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText() error: %v", err)
	}

	if string(data) != "" {
		t.Errorf("zero CanonicalID.MarshalText() = %q, want empty", string(data))
	}
}

func TestCanonicalID_UnmarshalText(t *testing.T) {
	var cid CanonicalID

	err := cid.UnmarshalText([]byte("business:alice@contoso.com"))
	if err != nil {
		t.Fatalf("UnmarshalText() error: %v", err)
	}

	if cid.String() != "business:alice@contoso.com" {
		t.Errorf("UnmarshalText result = %q, want %q", cid.String(), "business:alice@contoso.com")
	}
}

func TestCanonicalID_UnmarshalText_Invalid(t *testing.T) {
	var cid CanonicalID

	err := cid.UnmarshalText([]byte("invalid"))
	if err == nil {
		t.Error("UnmarshalText(\"invalid\") should return error")
	}
}

func TestCanonicalID_TextRoundTrip(t *testing.T) {
	original := MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs")

	data, err := original.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText() error: %v", err)
	}

	var restored CanonicalID
	if err := restored.UnmarshalText(data); err != nil {
		t.Fatalf("UnmarshalText() error: %v", err)
	}

	if original.String() != restored.String() {
		t.Errorf("round-trip failed: original=%q, restored=%q", original.String(), restored.String())
	}
}
