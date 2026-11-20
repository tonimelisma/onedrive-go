package cli

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

const (
	accountViewTestReadyUser  = "Ready User"
	accountViewTestDriveReady = "drive-ready"
	accountViewTestOrphanUser = "Orphan User"
)

func requireAccountViewByEmail(t *testing.T, views []accountView, email string) accountView {
	t.Helper()

	for i := range views {
		if views[i].Email == email {
			return views[i]
		}
	}

	require.FailNow(t, "account view not found", "email=%s", email)
	return accountView{}
}

// Validates: R-3.1.5, R-2.10.47
func TestBuildAccountViews_ConfiguredAccountWithUsableSavedLoginIsReady(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:ready@example.com")
	cfg := config.DefaultConfig()
	cfg.Drives[cid] = config.Drive{SyncDir: "~/OneDrive"}
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_ready@example.com.json")
	seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
		account.DisplayName = accountViewTestReadyUser
	})
	seedCatalogDrive(t, cid, func(drive *config.CatalogDrive) {
		drive.RemoteDriveID = accountViewTestDriveReady
	})
	stored, err := config.LoadCatalog()
	require.NoError(t, err)

	views, err := buildAccountViews(t.Context(), cfg, stored, testDriveLogger(t))
	require.NoError(t, err)
	entry := requireAccountViewByEmail(t, views, "ready@example.com")

	assert.True(t, entry.Configured)
	assert.Equal(t, driveid.DriveTypePersonal, entry.DriveType)
	assert.Equal(t, accountViewTestReadyUser, entry.DisplayName)
	assert.Empty(t, entry.SavedLoginReason)
	assert.Empty(t, entry.AuthRequirementReason)
	assert.Equal(t, authstate.StateReady, entry.AuthHealth.State)
}

// Validates: R-3.1.5
func TestBuildAccountViews_OrphanedProfileWithoutTokenRequiresAuthentication(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:orphan@example.com")
	seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
		account.DisplayName = accountViewTestOrphanUser
	})
	require.NoError(t, touchStateDBForAccount(t, cid))
	stored, err := config.LoadCatalog()
	require.NoError(t, err)

	views, err := buildAccountViews(t.Context(), config.DefaultConfig(), stored, testDriveLogger(t))
	require.NoError(t, err)
	entry := requireAccountViewByEmail(t, views, "orphan@example.com")

	assert.False(t, entry.Configured)
	assert.Equal(t, accountViewTestOrphanUser, entry.DisplayName)
	assert.Equal(t, authstate.ReasonMissingLogin, entry.SavedLoginReason)
	assert.Equal(t, 1, entry.StateDBCount)
	assert.Equal(t, authstate.StateAuthenticationRequired, entry.AuthHealth.State)
	assert.Equal(t, authstate.ReasonMissingLogin, entry.AuthHealth.Reason)
}

// Validates: R-3.3.2, R-3.3.9
func TestBuildAccountViews_TokenBackedCatalogAccountIsIncluded(t *testing.T) {
	setTestDriveHome(t)

	writeTestTokenFile(t, config.DefaultDataDir(), "token_business_discovered@contoso.com.json")
	stored, err := config.LoadCatalog()
	require.NoError(t, err)

	views, err := buildAccountViews(t.Context(), config.DefaultConfig(), stored, testDriveLogger(t))
	require.NoError(t, err)
	entry := requireAccountViewByEmail(t, views, "discovered@contoso.com")

	assert.False(t, entry.Configured)
	assert.Equal(t, driveid.DriveTypeBusiness, entry.DriveType)
	assert.Empty(t, entry.SavedLoginReason)
	assert.Equal(t, authstate.StateReady, entry.AuthHealth.State)
}

// Validates: R-3.1.5
func TestBuildAccountViews_StateDBWithoutCatalogIsExcluded(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:orphan@example.com")
	require.NoError(t, touchStateDBForAccount(t, cid))
	stored, err := config.LoadCatalog()
	require.NoError(t, err)

	views, err := buildAccountViews(t.Context(), config.DefaultConfig(), stored, testDriveLogger(t))
	require.NoError(t, err)
	assert.Empty(t, views)
}

// Validates: R-2.10.45, R-2.10.47
func TestBuildAccountViews_PersistedAuthScopeWinsOnlyWhenSavedLoginIsOtherwiseUsable(t *testing.T) {
	setTestDriveHome(t)

	usableCID := driveid.MustCanonicalID("personal:usable@example.com")
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_usable@example.com.json")
	seedAuthScope(t, usableCID)

	missingCID := driveid.MustCanonicalID("personal:missing@example.com")
	seedCatalogAccount(t, missingCID, func(account *config.CatalogAccount) {
		account.DisplayName = "Missing User"
	})
	seedAuthScope(t, missingCID)
	stored, err := config.LoadCatalog()
	require.NoError(t, err)

	views, err := buildAccountViews(t.Context(), config.DefaultConfig(), stored, testDriveLogger(t))
	require.NoError(t, err)

	usable := requireAccountViewByEmail(t, views, "usable@example.com")
	assert.Empty(t, usable.SavedLoginReason)
	assert.Equal(t, authstate.ReasonSyncAuthRejected, usable.AuthRequirementReason)
	assert.Equal(t, authstate.ReasonSyncAuthRejected, usable.AuthHealth.Reason)

	missing := requireAccountViewByEmail(t, views, "missing@example.com")
	assert.Equal(t, authstate.ReasonMissingLogin, missing.SavedLoginReason)
	assert.Equal(t, authstate.ReasonSyncAuthRejected, missing.AuthRequirementReason)
	assert.Equal(t, authstate.ReasonMissingLogin, missing.AuthHealth.Reason)
}

// A personal and a work or school account at one address share one view. It
// speaks for the first of them by canonical ID, the account whose saved login
// it checks and signs in with: its name, its drive type and its sign-in
// requirement, whichever of the two needs a sign-in. The other account adds
// its saved login and nothing else.
//
// Validates: R-2.10.45, R-2.10.47
func TestBuildAccountViews_TwoAccountsAtOneAddressShowTheFirstOnesRequirement(t *testing.T) {
	business := driveid.MustCanonicalID("business:twin@example.com")
	personal := driveid.MustCanonicalID("personal:twin@example.com")

	for _, marked := range []driveid.CanonicalID{business, personal} {
		t.Run(marked.DriveType(), func(t *testing.T) {
			setTestDriveHome(t)
			for _, account := range []driveid.CanonicalID{business, personal} {
				writeTestTokenFile(t, config.DefaultDataDir(), "token_"+account.DriveType()+"_"+account.Email()+".json")
				seedCatalogAccount(t, account, func(record *config.CatalogAccount) {
					record.DisplayName = account.DriveType() + " user"
				})
			}
			require.NoError(t, config.MarkAccountAuthRequired(config.DefaultDataDir(), marked, authstate.ReasonSyncAuthRejected))
			stored, err := config.LoadCatalog()
			require.NoError(t, err)

			views, err := buildAccountViews(t.Context(), config.DefaultConfig(), stored, testDriveLogger(t))
			require.NoError(t, err)
			require.Len(t, views, 1)
			entry := views[0]

			assert.Equal(t, business, entry.AccountCanonicalID)
			assert.Equal(t, business, entry.RepresentativeTokenID, "the view signs in as the account it speaks for")
			assert.Equal(t, driveid.DriveTypeBusiness, entry.DriveType)
			assert.Equal(t, "business user", entry.DisplayName)
			assert.Equal(t, []driveid.CanonicalID{business, personal}, entry.TokenDriveIDs)
			if marked == business {
				assert.Equal(t, authstate.ReasonSyncAuthRejected, entry.AuthRequirementReason)
				assert.Equal(t, authstate.StateAuthenticationRequired, entry.AuthHealth.State)
			} else {
				assert.Empty(t, entry.AuthRequirementReason, "the personal account's requirement is not the work account's")
				assert.Equal(t, authstate.StateReady, entry.AuthHealth.State)
			}
		})
	}
}

func touchStateDBForAccount(t *testing.T, cid driveid.CanonicalID) error {
	t.Helper()

	store, err := syncstore.NewSyncStore(t.Context(), config.DriveStatePath(cid), testDriveLogger(t))
	require.NoError(t, err)
	defer func() {
		require.NoError(t, store.Close(t.Context()))
	}()

	if err := store.UpsertBlockScope(t.Context(), &syncdomain.BlockScope{
		Key:           syncdomain.SKService(),
		TrialInterval: 5 * time.Second,
		NextTrialAt:   time.Date(2026, 4, 3, 8, 0, 5, 0, time.UTC),
	}); err != nil {
		return fmt.Errorf("upsert block scope: %w", err)
	}

	return nil
}
