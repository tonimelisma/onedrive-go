package config

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func TestDefaultDisplayName_Personal(t *testing.T) {
	cid := driveid.MustCanonicalID("personal:me@outlook.com")
	assert.Equal(t, "me@outlook.com", DefaultDisplayName(cid))
}

func TestDefaultDisplayName_Business(t *testing.T) {
	cid := driveid.MustCanonicalID("business:alice@contoso.com")
	assert.Equal(t, "alice@contoso.com", DefaultDisplayName(cid))
}

func TestDefaultDisplayName_SharePoint(t *testing.T) {
	cid := driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs")
	assert.Equal(t, "marketing / Docs", DefaultDisplayName(cid))
}

func TestDefaultDisplayName_SharePointNoSiteLibrary(t *testing.T) {
	cid := driveid.MustCanonicalID("sharepoint:alice@contoso.com")
	assert.Equal(t, "alice@contoso.com", DefaultDisplayName(cid))
}

func TestDefaultDisplayName_Shared(t *testing.T) {
	cid := driveid.MustCanonicalID("shared:me@outlook.com:b!TG9yZW0:01ABCDEF")
	name := DefaultDisplayName(cid)
	// Shared drives use a placeholder — the CLI will override with API data.
	assert.NotEmpty(t, name)
	assert.Contains(t, name, "b!TG9yZW0")
}
