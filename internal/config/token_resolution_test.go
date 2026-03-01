package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func TestTokenCanonicalID_Personal(t *testing.T) {
	cid := driveid.MustCanonicalID("personal:user@example.com")
	got, err := TokenCanonicalID(cid, nil)
	require.NoError(t, err)
	assert.Equal(t, "personal:user@example.com", got.String())
}

func TestTokenCanonicalID_Business(t *testing.T) {
	cid := driveid.MustCanonicalID("business:alice@contoso.com")
	got, err := TokenCanonicalID(cid, nil)
	require.NoError(t, err)
	assert.Equal(t, "business:alice@contoso.com", got.String())
}

func TestTokenCanonicalID_SharePoint(t *testing.T) {
	cid := driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs")
	got, err := TokenCanonicalID(cid, nil)
	require.NoError(t, err)
	assert.Equal(t, "business:alice@contoso.com", got.String())
}

func TestTokenCanonicalID_SharedWithPersonalAccount(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:me@outlook.com")] = Drive{SyncDir: "~/OneDrive"}

	cid := driveid.MustCanonicalID("shared:me@outlook.com:b!TG9yZW0:01ABCDEF")
	got, err := TokenCanonicalID(cid, cfg)
	require.NoError(t, err)
	assert.Equal(t, "personal:me@outlook.com", got.String())
}

func TestTokenCanonicalID_SharedWithBusinessAccount(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: "~/OneDrive"}

	cid := driveid.MustCanonicalID("shared:alice@contoso.com:b!TG9yZW0:01ABCDEF")
	got, err := TokenCanonicalID(cid, cfg)
	require.NoError(t, err)
	assert.Equal(t, "business:alice@contoso.com", got.String())
}

func TestTokenCanonicalID_SharedNoMatchingAccount(t *testing.T) {
	cfg := DefaultConfig()
	// No drives configured for this email.
	cid := driveid.MustCanonicalID("shared:nobody@example.com:b!TG9yZW0:01ABCDEF")
	_, err := TokenCanonicalID(cid, cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "nobody@example.com")
}

func TestTokenCanonicalID_SharedNilConfig(t *testing.T) {
	cid := driveid.MustCanonicalID("shared:me@outlook.com:b!TG9yZW0:01ABCDEF")
	_, err := TokenCanonicalID(cid, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "config required")
}
