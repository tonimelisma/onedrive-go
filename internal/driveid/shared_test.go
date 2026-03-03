package driveid

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, cid.String())
		})
	}
}

func TestNewCanonicalID_SharedFieldRouting(t *testing.T) {
	cid, err := NewCanonicalID("shared:me@outlook.com:b!TG9yZW0:01ABCDEF")
	require.NoError(t, err)

	assert.Equal(t, "shared", cid.DriveType())
	assert.Equal(t, "me@outlook.com", cid.Email())
	assert.Equal(t, "b!TG9yZW0", cid.SourceDriveID())
	assert.Equal(t, "01ABCDEF", cid.SourceItemID())

	// Shared fields must NOT leak into SharePoint-specific fields.
	assert.Empty(t, cid.Site(), "Site() should be empty for shared type")
	assert.Empty(t, cid.Library(), "Library() should be empty for shared type")
}

func TestNewCanonicalID_PersonalRejectsExtraParts(t *testing.T) {
	_, err := NewCanonicalID("personal:user@example.com:extra")
	assert.Error(t, err)
}

func TestNewCanonicalID_BusinessRejectsExtraParts(t *testing.T) {
	_, err := NewCanonicalID("business:alice@contoso.com:extra:parts")
	assert.Error(t, err)
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
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, cid.String())
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
			assert.Equal(t, tt.want, tt.cid.IsShared())
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
			assert.Equal(t, tt.want, tt.cid.IsPersonal())
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
			assert.Equal(t, tt.want, tt.cid.IsBusiness())
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
			assert.Equal(t, tt.want, tt.cid.SourceDriveID())
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
			assert.Equal(t, tt.want, tt.cid.SourceItemID())
		})
	}
}

func TestCanonicalID_SharedTextRoundTrip(t *testing.T) {
	raw := "shared:me@outlook.com:b!TG9yZW0:01ABCDEF"
	original := MustCanonicalID(raw)

	data, err := original.MarshalText()
	require.NoError(t, err)

	var restored CanonicalID
	err = restored.UnmarshalText(data)
	require.NoError(t, err)

	assert.Equal(t, original.String(), restored.String())
	assert.Equal(t, original.SourceDriveID(), restored.SourceDriveID())
	assert.Equal(t, original.SourceItemID(), restored.SourceItemID())
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
			assert.Equal(t, tt.want, tt.a.Equal(tt.b))
		})
	}
}

func TestCanonicalID_SharedFieldIsolation(t *testing.T) {
	// Verify that shared fields (sourceDriveID, sourceItemID) don't leak
	// into SharePoint fields (site, library) and vice versa.
	shared := MustCanonicalID("shared:me@outlook.com:b!TG9yZW0:01ABCDEF")
	sp := MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs")

	// Shared type must not expose site/library.
	assert.Empty(t, shared.Site())
	assert.Empty(t, shared.Library())

	// SharePoint must not expose sourceDriveID/sourceItemID.
	assert.Empty(t, sp.SourceDriveID())
	assert.Empty(t, sp.SourceItemID())
}

// TokenCanonicalID() was removed from CanonicalID (B-273).
// Shared drive token resolution is tested in config/token_resolution_test.go.

func TestID_Equal_ZeroValues(t *testing.T) {
	// Both are "zero" per IsZero() — they must be Equal.
	empty := New("")
	zeros := New("0")
	structZero := ID{}

	assert.True(t, empty.Equal(structZero), "New(\"\").Equal(ID{}) must be true — both are zero")
	assert.True(t, zeros.Equal(structZero), "New(\"0\").Equal(ID{}) must be true — both are zero")
	assert.True(t, empty.Equal(zeros), "New(\"\").Equal(New(\"0\")) must be true — both are zero")
}

func TestIsValidDriveType_Shared(t *testing.T) {
	assert.True(t, IsValidDriveType("shared"))
}
