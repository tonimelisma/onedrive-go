package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

func TestConstructCanonicalID(t *testing.T) {
	tests := []struct {
		driveType string
		email     string
		want      string
	}{
		{"personal", "toni@outlook.com", "personal:toni@outlook.com"},
		{"business", "alice@contoso.com", "business:alice@contoso.com"},
		{"documentLibrary", "bob@example.org", "documentLibrary:bob@example.org"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := constructCanonicalID(tt.driveType, tt.email)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAccountEmailFromCanonicalID(t *testing.T) {
	tests := []struct {
		id   string
		want string
	}{
		{"personal:toni@outlook.com", "toni@outlook.com"},
		{"business:alice@contoso.com", "alice@contoso.com"},
		{"sharepoint:alice@contoso.com:marketing:Docs", "alice@contoso.com:marketing:Docs"},
		{"nocolon", "nocolon"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			got := accountEmailFromCanonicalID(tt.id)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDriveTypeFromCanonicalID(t *testing.T) {
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
			got := driveTypeFromCanonicalID(tt.id)
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

	// Note: accountEmailFromCanonicalID("sharepoint:alice@example.com:marketing:Docs")
	// returns "alice@example.com:marketing:Docs" -- not matching "alice@example.com".
	// This is correct: SharePoint canonical IDs have a different email extraction.
	drives := drivesForAccount(cfg, "alice@example.com")

	assert.Len(t, drives, 2)
	assert.Contains(t, drives, "personal:alice@example.com")
	assert.Contains(t, drives, "business:alice@example.com")
}

func TestPrintLoginSuccess_DoesNotPanic(t *testing.T) {
	// Verify the print functions don't panic with various inputs.
	// Output goes to stdout, which is fine in tests.
	printLoginSuccess("personal", "toni@outlook.com", "", "personal:toni@outlook.com", "~/OneDrive")
	printLoginSuccess("business", "alice@contoso.com", "Contoso Ltd", "business:alice@contoso.com", "~/OneDrive - Contoso")
	printLoginSuccess("business", "bob@example.com", "", "business:bob@example.com", "~/OneDrive - Business")
	printLoginSuccess("documentLibrary", "carol@example.com", "", "documentLibrary:carol@example.com", "~/SP")
}
