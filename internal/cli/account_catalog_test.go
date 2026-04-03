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
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func accountCatalogEntryByEmail(t *testing.T, catalog []accountCatalogEntry, email string) accountCatalogEntry {
	t.Helper()

	for i := range catalog {
		if catalog[i].Email == email {
			return catalog[i]
		}
	}

	require.FailNow(t, "catalog entry not found", "email=%s", email)
	return accountCatalogEntry{}
}

// Validates: R-3.1.5, R-2.10.47
func TestBuildAccountCatalog_ConfiguredAccountWithUsableSavedLoginIsReady(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:ready@example.com")
	cfg := config.DefaultConfig()
	cfg.Drives[cid] = config.Drive{SyncDir: "~/OneDrive"}
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_ready@example.com.json")
	require.NoError(t, config.SaveAccountProfile(cid, &config.AccountProfile{
		DisplayName: "Ready User",
	}))

	catalog := buildAccountCatalog(t.Context(), cfg, testDriveLogger(t))
	entry := accountCatalogEntryByEmail(t, catalog, "ready@example.com")

	assert.True(t, entry.Configured)
	assert.Equal(t, driveid.DriveTypePersonal, entry.DriveType)
	assert.Equal(t, "Ready User", entry.DisplayName)
	assert.Equal(t, savedLoginStateUsable, entry.SavedLoginState)
	assert.False(t, entry.HasPersistedAuthScope)
	assert.Equal(t, authstate.StateReady, entry.AuthHealth.State)
}

// Validates: R-3.1.5
func TestBuildAccountCatalog_OrphanedProfileWithoutTokenRequiresAuthentication(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:orphan@example.com")
	require.NoError(t, config.SaveAccountProfile(cid, &config.AccountProfile{
		DisplayName: "Orphan User",
	}))
	require.NoError(t, touchStateDBForAccount(t, cid))

	catalog := buildAccountCatalog(t.Context(), config.DefaultConfig(), testDriveLogger(t))
	entry := accountCatalogEntryByEmail(t, catalog, "orphan@example.com")

	assert.False(t, entry.Configured)
	assert.Equal(t, "Orphan User", entry.DisplayName)
	assert.Equal(t, savedLoginStateMissing, entry.SavedLoginState)
	assert.Equal(t, 1, entry.StateDBCount)
	assert.Equal(t, authstate.StateAuthenticationRequired, entry.AuthHealth.State)
	assert.Equal(t, authstate.ReasonMissingLogin, entry.AuthHealth.Reason)
}

// Validates: R-3.3.2, R-3.3.9
func TestBuildAccountCatalog_TokenWithoutConfigIsIncluded(t *testing.T) {
	setTestDriveHome(t)

	writeTestTokenFile(t, config.DefaultDataDir(), "token_business_discovered@contoso.com.json")

	catalog := buildAccountCatalog(t.Context(), config.DefaultConfig(), testDriveLogger(t))
	entry := accountCatalogEntryByEmail(t, catalog, "discovered@contoso.com")

	assert.False(t, entry.Configured)
	assert.Equal(t, driveid.DriveTypeBusiness, entry.DriveType)
	assert.Equal(t, savedLoginStateUsable, entry.SavedLoginState)
	assert.Equal(t, authstate.StateReady, entry.AuthHealth.State)
}

// Validates: R-2.10.45, R-2.10.47
func TestBuildAccountCatalog_PersistedAuthScopeWinsOnlyWhenSavedLoginIsOtherwiseUsable(t *testing.T) {
	setTestDriveHome(t)

	usableCID := driveid.MustCanonicalID("personal:usable@example.com")
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_usable@example.com.json")
	seedAuthScope(t, usableCID)

	missingCID := driveid.MustCanonicalID("personal:missing@example.com")
	require.NoError(t, config.SaveAccountProfile(missingCID, &config.AccountProfile{
		DisplayName: "Missing User",
	}))
	seedAuthScope(t, missingCID)

	catalog := buildAccountCatalog(t.Context(), config.DefaultConfig(), testDriveLogger(t))

	usable := accountCatalogEntryByEmail(t, catalog, "usable@example.com")
	assert.Equal(t, savedLoginStateUsable, usable.SavedLoginState)
	assert.True(t, usable.HasPersistedAuthScope)
	assert.Equal(t, authstate.ReasonSyncAuthRejected, usable.AuthHealth.Reason)

	missing := accountCatalogEntryByEmail(t, catalog, "missing@example.com")
	assert.Equal(t, savedLoginStateMissing, missing.SavedLoginState)
	assert.True(t, missing.HasPersistedAuthScope)
	assert.Equal(t, authstate.ReasonMissingLogin, missing.AuthHealth.Reason)
}

func touchStateDBForAccount(t *testing.T, cid driveid.CanonicalID) error {
	t.Helper()

	store, err := syncstore.NewSyncStore(t.Context(), config.DriveStatePath(cid), testDriveLogger(t))
	require.NoError(t, err)
	defer func() {
		require.NoError(t, store.Close(t.Context()))
	}()

	if err := store.UpsertScopeBlock(t.Context(), &synctypes.ScopeBlock{
		Key:           synctypes.SKService(),
		IssueType:     synctypes.IssueServiceOutage,
		TimingSource:  synctypes.ScopeTimingBackoff,
		BlockedAt:     time.Date(2026, 4, 3, 8, 0, 0, 0, time.UTC),
		TrialInterval: 5 * time.Second,
		NextTrialAt:   time.Date(2026, 4, 3, 8, 0, 5, 0, time.UTC),
	}); err != nil {
		return fmt.Errorf("upsert scope block: %w", err)
	}

	return nil
}
