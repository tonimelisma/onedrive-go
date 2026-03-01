package driveid

import (
	"testing"
)

// --- RED tests for B-272 + foundation fixes ---
// All of these tests should FAIL before the GREEN implementation.

func TestNewCanonicalID_SharedType(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{
			name: "shared with all parts",
			raw:  "shared:me@outlook.com:b!TG9yZW0:01ABCDEF",
			want: "shared:me@outlook.com:b!TG9yZW0:01ABCDEF",
		},
		{
			name:    "shared missing source item ID",
			raw:     "shared:me@outlook.com:b!TG9yZW0",
			wantErr: true,
		},
		{
			name:    "shared missing source drive and item IDs",
			raw:     "shared:me@outlook.com",
			wantErr: true,
		},
		{
			name:    "shared empty email",
			raw:     "shared:",
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

func TestNewCanonicalID_SharedFieldRouting(t *testing.T) {
	cid, err := NewCanonicalID("shared:me@outlook.com:b!TG9yZW0:01ABCDEF")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cid.DriveType() != "shared" {
		t.Errorf("DriveType() = %q, want %q", cid.DriveType(), "shared")
	}

	if cid.Email() != "me@outlook.com" {
		t.Errorf("Email() = %q, want %q", cid.Email(), "me@outlook.com")
	}

	if cid.SourceDriveID() != "b!TG9yZW0" {
		t.Errorf("SourceDriveID() = %q, want %q", cid.SourceDriveID(), "b!TG9yZW0")
	}

	if cid.SourceItemID() != "01ABCDEF" {
		t.Errorf("SourceItemID() = %q, want %q", cid.SourceItemID(), "01ABCDEF")
	}

	// Shared fields must NOT leak into SharePoint-specific fields.
	if cid.Site() != "" {
		t.Errorf("Site() = %q, want empty for shared type", cid.Site())
	}

	if cid.Library() != "" {
		t.Errorf("Library() = %q, want empty for shared type", cid.Library())
	}
}

func TestNewCanonicalID_PersonalRejectsExtraParts(t *testing.T) {
	_, err := NewCanonicalID("personal:user@example.com:extra")
	if err == nil {
		t.Error("expected error for personal with extra parts, got nil")
	}
}

func TestNewCanonicalID_BusinessRejectsExtraParts(t *testing.T) {
	_, err := NewCanonicalID("business:alice@contoso.com:extra:parts")
	if err == nil {
		t.Error("expected error for business with extra parts, got nil")
	}
}

func TestConstructShared(t *testing.T) {
	tests := []struct {
		name          string
		email         string
		sourceDriveID string
		sourceItemID  string
		want          string
		wantErr       bool
	}{
		{
			name:          "valid shared",
			email:         "me@outlook.com",
			sourceDriveID: "b!TG9yZW0",
			sourceItemID:  "01ABCDEF",
			want:          "shared:me@outlook.com:b!TG9yZW0:01ABCDEF",
		},
		{
			name:          "empty email",
			email:         "",
			sourceDriveID: "b!TG9yZW0",
			sourceItemID:  "01ABCDEF",
			wantErr:       true,
		},
		{
			name:          "empty source drive ID",
			email:         "me@outlook.com",
			sourceDriveID: "",
			sourceItemID:  "01ABCDEF",
			wantErr:       true,
		},
		{
			name:          "empty source item ID",
			email:         "me@outlook.com",
			sourceDriveID: "b!TG9yZW0",
			sourceItemID:  "",
			wantErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cid, err := ConstructShared(tt.email, tt.sourceDriveID, tt.sourceItemID)
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
				t.Errorf("ConstructShared() = %q, want %q", cid.String(), tt.want)
			}
		})
	}
}

func TestCanonicalID_IsShared(t *testing.T) {
	tests := []struct {
		name string
		cid  CanonicalID
		want bool
	}{
		{"shared is shared", MustCanonicalID("shared:me@outlook.com:b!TG9yZW0:01ABCDEF"), true},
		{"personal is not shared", MustCanonicalID("personal:user@example.com"), false},
		{"business is not shared", MustCanonicalID("business:alice@contoso.com"), false},
		{"sharepoint is not shared", MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cid.IsShared(); got != tt.want {
				t.Errorf("IsShared() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCanonicalID_IsPersonal(t *testing.T) {
	tests := []struct {
		name string
		cid  CanonicalID
		want bool
	}{
		{"personal", MustCanonicalID("personal:user@example.com"), true},
		{"business", MustCanonicalID("business:alice@contoso.com"), false},
		{"sharepoint", MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cid.IsPersonal(); got != tt.want {
				t.Errorf("IsPersonal() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCanonicalID_IsBusiness(t *testing.T) {
	tests := []struct {
		name string
		cid  CanonicalID
		want bool
	}{
		{"business", MustCanonicalID("business:alice@contoso.com"), true},
		{"personal", MustCanonicalID("personal:user@example.com"), false},
		{"sharepoint", MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cid.IsBusiness(); got != tt.want {
				t.Errorf("IsBusiness() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCanonicalID_SourceDriveID(t *testing.T) {
	tests := []struct {
		name string
		cid  CanonicalID
		want string
	}{
		{"shared returns source drive ID", MustCanonicalID("shared:me@outlook.com:b!TG9yZW0:01ABCDEF"), "b!TG9yZW0"},
		{"personal returns empty", MustCanonicalID("personal:user@example.com"), ""},
		{"business returns empty", MustCanonicalID("business:alice@contoso.com"), ""},
		{"sharepoint returns empty", MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"), ""},
		{"zero value returns empty", CanonicalID{}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cid.SourceDriveID(); got != tt.want {
				t.Errorf("SourceDriveID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCanonicalID_SourceItemID(t *testing.T) {
	tests := []struct {
		name string
		cid  CanonicalID
		want string
	}{
		{"shared returns source item ID", MustCanonicalID("shared:me@outlook.com:b!TG9yZW0:01ABCDEF"), "01ABCDEF"},
		{"personal returns empty", MustCanonicalID("personal:user@example.com"), ""},
		{"business returns empty", MustCanonicalID("business:alice@contoso.com"), ""},
		{"sharepoint returns empty", MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"), ""},
		{"zero value returns empty", CanonicalID{}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cid.SourceItemID(); got != tt.want {
				t.Errorf("SourceItemID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCanonicalID_SharedTextRoundTrip(t *testing.T) {
	raw := "shared:me@outlook.com:b!TG9yZW0:01ABCDEF"
	original := MustCanonicalID(raw)

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

	if original.SourceDriveID() != restored.SourceDriveID() {
		t.Errorf("SourceDriveID mismatch: %q vs %q", original.SourceDriveID(), restored.SourceDriveID())
	}

	if original.SourceItemID() != restored.SourceItemID() {
		t.Errorf("SourceItemID mismatch: %q vs %q", original.SourceItemID(), restored.SourceItemID())
	}
}

func TestCanonicalID_Equal(t *testing.T) {
	tests := []struct {
		name string
		a    CanonicalID
		b    CanonicalID
		want bool
	}{
		{"same personal", MustCanonicalID("personal:user@example.com"), MustCanonicalID("personal:user@example.com"), true},
		{"different email", MustCanonicalID("personal:a@example.com"), MustCanonicalID("personal:b@example.com"), false},
		{"different type", MustCanonicalID("personal:user@example.com"), MustCanonicalID("business:user@example.com"), false},
		{"same shared", MustCanonicalID("shared:me@outlook.com:b!TG9yZW0:01ABCDEF"), MustCanonicalID("shared:me@outlook.com:b!TG9yZW0:01ABCDEF"), true},
		{"different shared item", MustCanonicalID("shared:me@outlook.com:b!TG9yZW0:01ABCDEF"), MustCanonicalID("shared:me@outlook.com:b!TG9yZW0:02GHIJKL"), false},
		{"both zero", CanonicalID{}, CanonicalID{}, true},
		{"zero vs non-zero", CanonicalID{}, MustCanonicalID("personal:user@example.com"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.Equal(tt.b); got != tt.want {
				t.Errorf("Equal() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCanonicalID_SharedFieldIsolation(t *testing.T) {
	// Verify that shared fields (sourceDriveID, sourceItemID) don't leak
	// into SharePoint fields (site, library) and vice versa.
	shared := MustCanonicalID("shared:me@outlook.com:b!TG9yZW0:01ABCDEF")
	sp := MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs")

	// Shared type must not expose site/library.
	if shared.Site() != "" {
		t.Errorf("shared.Site() = %q, want empty", shared.Site())
	}

	if shared.Library() != "" {
		t.Errorf("shared.Library() = %q, want empty", shared.Library())
	}

	// SharePoint must not expose sourceDriveID/sourceItemID.
	if sp.SourceDriveID() != "" {
		t.Errorf("sp.SourceDriveID() = %q, want empty", sp.SourceDriveID())
	}

	if sp.SourceItemID() != "" {
		t.Errorf("sp.SourceItemID() = %q, want empty", sp.SourceItemID())
	}
}

func TestCanonicalID_TokenCanonicalID_Shared(t *testing.T) {
	// Shared drives return self — callers must use config.TokenCanonicalID()
	// instead once B-273 lands. For now, returning self is the safe default.
	shared := MustCanonicalID("shared:me@outlook.com:b!TG9yZW0:01ABCDEF")
	token := shared.TokenCanonicalID()

	if token.String() != "shared:me@outlook.com:b!TG9yZW0:01ABCDEF" {
		t.Errorf("TokenCanonicalID() = %q, want self for shared (B-273 will change this)", token.String())
	}
}

func TestID_Equal_ZeroValues(t *testing.T) {
	// Both are "zero" per IsZero() — they must be Equal.
	empty := New("")
	zeros := New("0")
	structZero := ID{}

	if !empty.Equal(structZero) {
		t.Error("New(\"\").Equal(ID{}) must be true — both are zero")
	}

	if !zeros.Equal(structZero) {
		t.Error("New(\"0\").Equal(ID{}) must be true — both are zero")
	}

	if !empty.Equal(zeros) {
		t.Error("New(\"\").Equal(New(\"0\")) must be true — both are zero")
	}
}

func TestIsValidDriveType_Shared(t *testing.T) {
	if !IsValidDriveType("shared") {
		t.Error("IsValidDriveType(\"shared\") must be true")
	}
}
