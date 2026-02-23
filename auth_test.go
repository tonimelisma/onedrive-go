package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

func TestEmailFromCanonicalString(t *testing.T) {
	tests := []struct {
		id   string
		want string
	}{
		{"personal:toni@outlook.com", "toni@outlook.com"},
		{"business:alice@contoso.com", "alice@contoso.com"},
		// SharePoint IDs have extra colon-separated segments after the email.
		{"sharepoint:alice@contoso.com:marketing:Docs", "alice@contoso.com"},
		{"nocolon", "nocolon"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			got := emailFromCanonicalString(tt.id)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDriveTypeFromCanonicalString(t *testing.T) {
	tests := []struct {
		id   string
		want string
	}{
		{"personal:toni@outlook.com", "personal"},
		{"business:alice@contoso.com", "business"},
		{"sharepoint:alice@contoso.com:marketing:Docs", "sharepoint"},
		{"nocolon", "nocolon"},
	}

	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			got := driveTypeFromCanonicalString(tt.id)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestUniqueAccounts(t *testing.T) {
	cfg := &config.Config{
		Drives: map[string]config.Drive{
			"personal:alice@example.com":   {},
			"business:alice@example.com":   {},
			"personal:bob@example.com":     {},
			"business:charlie@example.com": {},
		},
	}

	accounts := uniqueAccounts(cfg)

	// Should have 3 unique emails (alice appears twice but only counted once).
	assert.Len(t, accounts, 3)
	assert.Contains(t, accounts, "alice@example.com")
	assert.Contains(t, accounts, "bob@example.com")
	assert.Contains(t, accounts, "charlie@example.com")
}

func TestCanonicalIDForToken(t *testing.T) {
	tests := []struct {
		name     string
		account  string
		driveIDs []string
		want     string
	}{
		{
			"personal drive",
			"alice@example.com",
			[]string{"personal:alice@example.com"},
			"personal:alice@example.com",
		},
		{
			"business preferred over sharepoint",
			"alice@contoso.com",
			[]string{"sharepoint:alice@contoso.com:site:lib", "business:alice@contoso.com"},
			"business:alice@contoso.com",
		},
		{
			"all sharepoint falls back to business prefix",
			"alice@contoso.com",
			[]string{"sharepoint:alice@contoso.com:site:lib"},
			"business:alice@contoso.com",
		},
		{
			"empty returns empty",
			"nobody@example.com",
			nil,
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := canonicalIDForToken(tt.account, tt.driveIDs)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDrivesForAccount(t *testing.T) {
	cfg := &config.Config{
		Drives: map[string]config.Drive{
			"personal:alice@example.com":                  {},
			"business:alice@example.com":                  {},
			"sharepoint:alice@example.com:marketing:Docs": {},
			"personal:bob@example.com":                    {},
		},
	}

	// With the fixed SplitN limit 3, SharePoint IDs now correctly extract the
	// email, so all three of alice's drives are returned.
	drives := drivesForAccount(cfg, "alice@example.com")

	assert.Len(t, drives, 3)
	assert.Contains(t, drives, "personal:alice@example.com")
	assert.Contains(t, drives, "business:alice@example.com")
	assert.Contains(t, drives, "sharepoint:alice@example.com:marketing:Docs")
}

func TestFindTokenFallback(t *testing.T) {
	// findTokenFallback probes the filesystem for existing token files.
	// We need to create temp files matching the token path pattern.
	// Since DriveTokenPath uses XDG paths, we test the logic by checking
	// that it returns the correct prefix based on which file exists.

	// With no token files on disk, should default to personal.
	got := findTokenFallback("nobody@example.com", slog.Default())
	assert.Equal(t, "personal:nobody@example.com", got)
}

func TestFindTokenFallback_PersonalExists(t *testing.T) {
	// Create a temp directory and a file matching the personal token path.
	personalID := "personal:test-fallback@example.com"
	personalPath := config.DriveTokenPath(personalID)

	if personalPath == "" {
		t.Skip("cannot determine token path on this platform")
	}

	// Create the directory and file.
	dir := filepath.Dir(personalPath)
	require.NoError(t, os.MkdirAll(dir, 0o700))

	require.NoError(t, os.WriteFile(personalPath, []byte("{}"), 0o600))
	t.Cleanup(func() { os.Remove(personalPath) })

	got := findTokenFallback("test-fallback@example.com", slog.Default())
	assert.Equal(t, personalID, got)
}

func TestFindTokenFallback_BusinessExists(t *testing.T) {
	// Create only a business token file — should return business prefix.
	businessID := "business:test-fallback-biz@example.com"
	businessPath := config.DriveTokenPath(businessID)

	if businessPath == "" {
		t.Skip("cannot determine token path on this platform")
	}

	dir := filepath.Dir(businessPath)
	require.NoError(t, os.MkdirAll(dir, 0o700))

	require.NoError(t, os.WriteFile(businessPath, []byte("{}"), 0o600))
	t.Cleanup(func() { os.Remove(businessPath) })

	got := findTokenFallback("test-fallback-biz@example.com", slog.Default())
	assert.Equal(t, businessID, got)
}

func TestPrintLoginSuccess_DoesNotPanic(t *testing.T) {
	// Verify the print functions don't panic with various inputs.
	// Output goes to stdout, which is fine in tests.
	printLoginSuccess("personal", "toni@outlook.com", "", "personal:toni@outlook.com", "~/OneDrive")
	printLoginSuccess("business", "alice@contoso.com", "Contoso Ltd", "business:alice@contoso.com", "~/OneDrive - Contoso")
	printLoginSuccess("business", "bob@example.com", "", "business:bob@example.com", "~/OneDrive - Business")
	printLoginSuccess("documentLibrary", "carol@example.com", "", "documentLibrary:carol@example.com", "~/SP")
}
