package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func TestAccountDriveType_PrefersNonSharePointAndFallsBack(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "business", accountDriveType([]driveid.CanonicalID{
		driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"),
		driveid.MustCanonicalID("business:alice@contoso.com"),
	}))
	assert.Equal(t, "sharepoint", accountDriveType([]driveid.CanonicalID{
		driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"),
	}))
	assert.Empty(t, accountDriveType(nil))
}

func TestAccountViewDriveType_PreferenceOrder(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "business", accountViewDriveType(&accountView{
		AccountCanonicalID: driveid.MustCanonicalID("business:alice@contoso.com"),
		ConfiguredDriveIDs: []driveid.CanonicalID{
			driveid.MustCanonicalID("personal:alice@example.com"),
		},
		TokenDriveIDs: []driveid.CanonicalID{
			driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"),
		},
	}))
	assert.Equal(t, "personal", accountViewDriveType(&accountView{
		ConfiguredDriveIDs: []driveid.CanonicalID{
			driveid.MustCanonicalID("personal:alice@example.com"),
		},
		TokenDriveIDs: []driveid.CanonicalID{
			driveid.MustCanonicalID("business:alice@contoso.com"),
		},
	}))
	assert.Equal(t, "sharepoint", accountViewDriveType(&accountView{
		TokenDriveIDs: []driveid.CanonicalID{
			driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"),
		},
	}))
	assert.Empty(t, accountViewDriveType(&accountView{}))
}
