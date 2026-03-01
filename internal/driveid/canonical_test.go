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
			name: "sharepoint with only site",
			raw:  "sharepoint:alice@contoso.com:marketing",
			want: "sharepoint:alice@contoso.com:marketing",
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

func TestNewCanonicalID_ParseOnceFields(t *testing.T) {
	// Verify that fields are parsed at construction time and stored.
	cid, err := NewCanonicalID("sharepoint:alice@contoso.com:marketing:Docs")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cid.driveType != "sharepoint" {
		t.Errorf("driveType = %q, want %q", cid.driveType, "sharepoint")
	}

	if cid.email != "alice@contoso.com" {
		t.Errorf("email = %q, want %q", cid.email, "alice@contoso.com")
	}

	if cid.site != "marketing" {
		t.Errorf("site = %q, want %q", cid.site, "marketing")
	}

	if cid.library != "Docs" {
		t.Errorf("library = %q, want %q", cid.library, "Docs")
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

func TestConstructSharePoint(t *testing.T) {
	tests := []struct {
		name    string
		email   string
		site    string
		library string
		want    string
		wantErr bool
	}{
		{
			name:    "valid sharepoint",
			email:   "alice@contoso.com",
			site:    "marketing",
			library: "Documents",
			want:    "sharepoint:alice@contoso.com:marketing:Documents",
		},
		{
			name:    "empty email",
			email:   "",
			site:    "marketing",
			library: "Documents",
			wantErr: true,
		},
		{
			name:    "empty site",
			email:   "alice@contoso.com",
			site:    "",
			library: "Documents",
			wantErr: true,
		},
		{
			name:    "empty library",
			email:   "alice@contoso.com",
			site:    "marketing",
			library: "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cid, err := ConstructSharePoint(tt.email, tt.site, tt.library)
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
				t.Errorf("ConstructSharePoint() = %q, want %q", cid.String(), tt.want)
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

// TokenCanonicalID() was removed from CanonicalID (B-273).
// Token resolution now lives in config.TokenCanonicalID(cid, cfg).
// See internal/config/token_resolution_test.go for coverage.

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

func TestCanonicalID_Site(t *testing.T) {
	tests := []struct {
		name string
		cid  CanonicalID
		want string
	}{
		{"personal returns empty", MustCanonicalID("personal:user@example.com"), ""},
		{"business returns empty", MustCanonicalID("business:alice@contoso.com"), ""},
		{"sharepoint returns site", MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"), "marketing"},
		{"sharepoint without site returns empty", MustCanonicalID("sharepoint:alice@contoso.com"), ""},
		{"zero value returns empty", CanonicalID{}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cid.Site(); got != tt.want {
				t.Errorf("Site() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCanonicalID_Library(t *testing.T) {
	tests := []struct {
		name string
		cid  CanonicalID
		want string
	}{
		{"personal returns empty", MustCanonicalID("personal:user@example.com"), ""},
		{"business returns empty", MustCanonicalID("business:alice@contoso.com"), ""},
		{"sharepoint returns library", MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"), "Docs"},
		{"sharepoint without site returns empty", MustCanonicalID("sharepoint:alice@contoso.com"), ""},
		{"sharepoint multi-word library", MustCanonicalID("sharepoint:alice@contoso.com:hr:Shared Documents"), "Shared Documents"},
		{"zero value returns empty", CanonicalID{}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cid.Library(); got != tt.want {
				t.Errorf("Library() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCanonicalID_TextRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"personal", "personal:user@example.com"},
		{"business", "business:alice@contoso.com"},
		{"sharepoint", "sharepoint:alice@contoso.com:marketing:Docs"},
		{"sharepoint multi-word library", "sharepoint:alice@contoso.com:hr:Shared Documents"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := MustCanonicalID(tt.raw)

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

			// Also verify parsed fields survive round-trip.
			if original.DriveType() != restored.DriveType() {
				t.Errorf("DriveType mismatch: %q vs %q", original.DriveType(), restored.DriveType())
			}

			if original.Email() != restored.Email() {
				t.Errorf("Email mismatch: %q vs %q", original.Email(), restored.Email())
			}

			if original.Site() != restored.Site() {
				t.Errorf("Site mismatch: %q vs %q", original.Site(), restored.Site())
			}

			if original.Library() != restored.Library() {
				t.Errorf("Library mismatch: %q vs %q", original.Library(), restored.Library())
			}
		})
	}
}

func TestCanonicalID_String_ZeroValue(t *testing.T) {
	var cid CanonicalID
	if cid.String() != "" {
		t.Errorf("zero-value String() = %q, want empty", cid.String())
	}
}
