package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
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

// A personal and a work or school account at one address have a view each:
// each speaks for its own account, with its own name, saved login, sign-in
// requirement and state databases, whichever of the two needs a sign-in.
//
// Validates: R-2.10.45, R-2.10.47, R-3.5.2
func TestBuildAccountViews_TwoAccountsAtOneAddressHaveAViewEach(t *testing.T) {
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
			require.NoError(t, touchStateDBForAccount(t, personal))
			require.NoError(t, config.MarkAccountAuthRequired(t.Context(), config.DefaultDataDir(), marked, authstate.ReasonSyncAuthRejected))
			stored, err := config.LoadCatalog()
			require.NoError(t, err)

			views, err := buildAccountViews(t.Context(), config.DefaultConfig(), stored, testDriveLogger(t))
			require.NoError(t, err)
			require.Len(t, views, 2, "one view per account")

			for i := range views {
				entry := views[i]
				account := []driveid.CanonicalID{business, personal}[i]
				assert.Equal(t, account, entry.AccountCanonicalID, "views sort by address, then canonical ID")
				assert.Equal(t, account, entry.RepresentativeTokenID, "each view signs in as its own account")
				assert.Equal(t, []driveid.CanonicalID{account}, entry.TokenDriveIDs)
				assert.Equal(t, account.DriveType(), entry.DriveType)
				assert.Equal(t, account.DriveType()+" user", entry.DisplayName)
				if account == marked {
					assert.Equal(t, authstate.ReasonSyncAuthRejected, entry.AuthRequirementReason)
					assert.Equal(t, authstate.StateAuthenticationRequired, entry.AuthHealth.State)
				} else {
					assert.Empty(t, entry.AuthRequirementReason, "the other account's requirement is not this one's")
					assert.Equal(t, authstate.StateReady, entry.AuthHealth.State)
				}
			}
			assert.Zero(t, views[0].StateDBCount, "the personal store is not the work account's")
			assert.Equal(t, 1, views[1].StateDBCount, "the personal store is the personal account's")
		})
	}
}

// Two accounts at one address that both need a sign-in are two status rows,
// in text and JSON, and two entries of drive list's accounts_requiring_auth.
//
// Validates: R-3.5.2, R-2.10.47, R-3.1.6
func TestStatus_TwoAccountsAtOneAddressAreTwoRowsAndTwoAuthEntries(t *testing.T) {
	setTestDriveHome(t)
	business := driveid.MustCanonicalID("business:twin-rows@example.com")
	personal := driveid.MustCanonicalID("personal:twin-rows@example.com")
	for _, account := range []driveid.CanonicalID{business, personal} {
		writeTestTokenFile(t, config.DefaultDataDir(), "token_"+account.DriveType()+"_"+account.Email()+".json")
		seedCatalogAccount(t, account, nil)
		require.NoError(t, config.MarkAccountAuthRequired(t.Context(), config.DefaultDataDir(), account,
			authstate.ReasonSyncAuthRejected))
	}
	stored, err := config.LoadCatalog()
	require.NoError(t, err)
	views, err := buildAccountViews(t.Context(), config.DefaultConfig(), stored, testDriveLogger(t))
	require.NoError(t, err)

	accounts := buildStatusAccountsFromViews(t.Context(), config.DefaultConfig(), nil, views, &mockSyncStateQuerier{})
	var jsonOut bytes.Buffer
	require.NoError(t, printStatusJSON(&jsonOut, accounts))
	var document statusOutput
	require.NoError(t, json.Unmarshal(jsonOut.Bytes(), &document))
	require.Len(t, document.Accounts, 2, "one row per account")
	for i := range document.Accounts {
		assert.Equal(t, business.Email(), document.Accounts[i].Email)
		assert.NotNil(t, document.Accounts[i].SignInRequired, "each row shows its own sign-in requirement")
	}
	assert.Equal(t, 2, document.Summary.AccountsRequiringAuth)

	var textOut bytes.Buffer
	require.NoError(t, printStatusText(&textOut, accounts))
	assert.Equal(t, 2, strings.Count(textOut.String(), "Account: "), "two account rows")
	assert.Equal(t, 2, strings.Count(textOut.String(), "Sign-in required:"))

	requirements := mergeAuthRequirements(accountViewAuthRequirements(views, func(accountView) bool { return true }))
	require.Len(t, requirements, 2, "one auth entry per account")
	assert.Equal(t, driveid.DriveTypeBusiness, requirements[0].DriveType)
	assert.Equal(t, driveid.DriveTypePersonal, requirements[1].DriveType)
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
