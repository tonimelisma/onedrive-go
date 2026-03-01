package driveid

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, cid.String())
		})
	}
}

func TestNewCanonicalID_ParseOnceFields(t *testing.T) {
	cid, err := NewCanonicalID("sharepoint:alice@contoso.com:marketing:Docs")
	require.NoError(t, err)

	assert.Equal(t, "sharepoint", cid.driveType)
	assert.Equal(t, "alice@contoso.com", cid.email)
	assert.Equal(t, "marketing", cid.site)
	assert.Equal(t, "Docs", cid.library)
}

func TestMustCanonicalID(t *testing.T) {
	t.Run("valid input", func(t *testing.T) {
		cid := MustCanonicalID("personal:user@example.com")
		assert.Equal(t, "personal:user@example.com", cid.String())
	})

	t.Run("invalid input panics", func(t *testing.T) {
		assert.Panics(t, func() {
			MustCanonicalID("invalid")
		})
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
		{
			name:      "sharepoint rejected with helpful message",
			driveType: "sharepoint",
			email:     "alice@contoso.com",
			wantErr:   true,
		},
		{
			name:      "shared rejected with helpful message",
			driveType: "shared",
			email:     "bob@example.com",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cid, err := Construct(tt.driveType, tt.email)
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, cid.String())
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
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, cid.String())
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
			assert.Equal(t, tt.want, tt.cid.DriveType())
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
			assert.Equal(t, tt.want, tt.cid.Email())
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
			assert.Equal(t, tt.want, tt.cid.IsSharePoint())
		})
	}
}

// TokenCanonicalID() was removed from CanonicalID (B-273).
// Token resolution now lives in config.TokenCanonicalID(cid, cfg).
// See internal/config/token_resolution_test.go for coverage.

func TestCanonicalID_IsZero(t *testing.T) {
	assert.True(t, CanonicalID{}.IsZero())
	assert.False(t, MustCanonicalID("personal:user@example.com").IsZero())
}

func TestCanonicalID_MarshalText(t *testing.T) {
	cid := MustCanonicalID("personal:user@example.com")
	data, err := cid.MarshalText()
	require.NoError(t, err)
	assert.Equal(t, "personal:user@example.com", string(data))
}

func TestCanonicalID_MarshalText_Zero(t *testing.T) {
	var cid CanonicalID
	data, err := cid.MarshalText()
	require.NoError(t, err)
	assert.Empty(t, string(data))
}

func TestCanonicalID_UnmarshalText(t *testing.T) {
	var cid CanonicalID
	err := cid.UnmarshalText([]byte("business:alice@contoso.com"))
	require.NoError(t, err)
	assert.Equal(t, "business:alice@contoso.com", cid.String())
}

func TestCanonicalID_UnmarshalText_Invalid(t *testing.T) {
	var cid CanonicalID
	err := cid.UnmarshalText([]byte("invalid"))
	require.Error(t, err)
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
			assert.Equal(t, tt.want, tt.cid.Site())
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
			assert.Equal(t, tt.want, tt.cid.Library())
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
		{"shared", "shared:bob@example.com:d!abc123:item456"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := MustCanonicalID(tt.raw)

			data, err := original.MarshalText()
			require.NoError(t, err)

			var restored CanonicalID
			require.NoError(t, restored.UnmarshalText(data))

			assert.Equal(t, original.String(), restored.String())
			assert.Equal(t, original.DriveType(), restored.DriveType())
			assert.Equal(t, original.Email(), restored.Email())
			assert.Equal(t, original.Site(), restored.Site())
			assert.Equal(t, original.Library(), restored.Library())
		})
	}
}

func TestCanonicalID_String_ZeroValue(t *testing.T) {
	var cid CanonicalID
	assert.Empty(t, cid.String())
}
