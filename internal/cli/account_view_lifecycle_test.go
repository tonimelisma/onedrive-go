package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

const (
	accountViewLifecycleMissingUser  = "Missing User"
	accountViewLifecycleInvalidUser  = "Invalid User"
	accountViewLifecycleRejectedUser = "Rejected User"
	accountViewLifecycleNoConfigUser = "No Config"
	accountViewLifecycleTestUser     = "Test User"
)

type accountViewLifecycleCase struct {
	name            string
	setup           func(*testing.T) (*config.Config, *config.Catalog)
	email           string
	wantSavedReason authstate.Reason
	wantAuthReason  authstate.Reason
	wantState       string
	wantConfigured  bool
	wantSelectable  bool
}

// Validates: R-3.1.5, R-2.10.45, R-2.10.47
func TestBuildAccountViews_AccountLifecycleMatrix(t *testing.T) {
	setTestDriveHome(t)

	cases := []accountViewLifecycleCase{
		readyAccountLifecycleCase(),
		missingAccountLifecycleCase(),
		invalidAccountLifecycleCase(),
		rejectedAccountLifecycleCase(),
		noConfiguredDriveLifecycleCase(),
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, stored := tc.setup(t)

			views := buildAccountViews(t.Context(), cfg, stored, testDriveLogger(t))
			view, found := accountViewByEmail(views, tc.email)
			require.True(t, found, "email=%s", tc.email)

			assert.Equal(t, tc.wantSavedReason, view.SavedLoginReason)
			assert.Equal(t, tc.wantAuthReason, view.AuthHealth.Reason)
			assert.Equal(t, tc.wantState, view.AuthHealth.State)
			assert.Equal(t, tc.wantConfigured, view.Configured)
			assert.Equal(t, tc.wantSelectable, accountLifecycle(&view).SelectableForLogout)
		})
	}
}

// Validates: R-3.1.5
func TestLoadAccountViewSnapshot_UsesValidatedStateInvariantErrors(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:user@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive"))
	seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
		account.UserID = snapshotTestUserID123
		account.DisplayName = accountViewLifecycleTestUser
	})

	_, err := loadAccountViewSnapshot(t.Context(), &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      cfgPath,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "configured drive")
	assert.Contains(t, err.Error(), "has no catalog entry")
}

// Validates: R-3.1.5
func TestDiscoverLiveDriveCatalog_DegradedDiscoveryDoesNotPersistAccountAuthRequirement(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:degraded@example.com")
	result := discoverLiveDriveCatalog(
		t.Context(),
		fakeStatusLiveDriveCatalogClient{
			drivesErr: errors.New("try later"),
			primary: &graph.Drive{
				ID:         driveid.New("drive-1"),
				Name:       "Primary",
				DriveType:  driveid.DriveTypePersonal,
				QuotaUsed:  1,
				QuotaTotal: 2,
			},
		},
		cid.Email(),
		"Degraded User",
		cid.DriveType(),
		testDriveLogger(t),
	)
	require.NotNil(t, result.Degraded)
	require.Len(t, result.LiveDrives, 1)
	assert.Equal(t, "Primary", result.LiveDrives[0].Name)
	assert.False(t, hasPersistedAccountAuthRequirement(t.Context(), cid.Email(), testDriveLogger(t)))
}

func writeInvalidTokenFile(path string) error {
	if err := os.WriteFile(path, []byte("{invalid"), 0o600); err != nil {
		return fmt.Errorf("write invalid token file: %w", err)
	}
	return nil
}

type fakeStatusLiveDriveCatalogClient struct {
	drivesErr error
	primary   *graph.Drive
}

func (f fakeStatusLiveDriveCatalogClient) Drives(context.Context) ([]graph.Drive, error) {
	return nil, f.drivesErr
}

func (f fakeStatusLiveDriveCatalogClient) PrimaryDrive(context.Context) (*graph.Drive, error) {
	return f.primary, nil
}

func readyAccountLifecycleCase() accountViewLifecycleCase {
	return accountViewLifecycleCase{
		name:            "known account with usable saved login",
		setup:           setupReadyAccountView,
		email:           "ready@example.com",
		wantState:       authstate.StateReady,
		wantConfigured:  true,
		wantSelectable:  true,
		wantSavedReason: "",
	}
}

func missingAccountLifecycleCase() accountViewLifecycleCase {
	return accountViewLifecycleCase{
		name:            "known account with missing saved login",
		setup:           setupMissingAccountView,
		email:           "missing@example.com",
		wantSavedReason: authstate.ReasonMissingLogin,
		wantAuthReason:  authstate.ReasonMissingLogin,
		wantState:       authstate.StateAuthenticationRequired,
		wantConfigured:  false,
		wantSelectable:  false,
	}
}

func invalidAccountLifecycleCase() accountViewLifecycleCase {
	return accountViewLifecycleCase{
		name:            "known account with invalid saved login",
		setup:           setupInvalidAccountView,
		email:           "invalid@example.com",
		wantSavedReason: authstate.ReasonInvalidSavedLogin,
		wantAuthReason:  authstate.ReasonInvalidSavedLogin,
		wantState:       authstate.StateAuthenticationRequired,
		wantConfigured:  false,
		wantSelectable:  false,
	}
}

func rejectedAccountLifecycleCase() accountViewLifecycleCase {
	return accountViewLifecycleCase{
		name:            "known account with persisted sync rejected",
		setup:           setupRejectedAccountView,
		email:           "rejected@example.com",
		wantAuthReason:  authstate.ReasonSyncAuthRejected,
		wantState:       authstate.StateAuthenticationRequired,
		wantConfigured:  false,
		wantSelectable:  true,
		wantSavedReason: "",
	}
}

func noConfiguredDriveLifecycleCase() accountViewLifecycleCase {
	return accountViewLifecycleCase{
		name:            "known account with zero configured drives",
		setup:           setupNoConfiguredDriveAccountView,
		email:           "noconfig@contoso.com",
		wantState:       authstate.StateReady,
		wantConfigured:  false,
		wantSelectable:  true,
		wantSavedReason: "",
	}
}

func setupReadyAccountView(t *testing.T) (*config.Config, *config.Catalog) {
	t.Helper()

	cid := driveid.MustCanonicalID("personal:ready@example.com")
	cfg := config.DefaultConfig()
	cfg.Drives[cid] = config.Drive{SyncDir: "~/OneDrive"}
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_ready@example.com.json")
	seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
		account.DisplayName = accountViewTestReadyUser
	})
	seedCatalogDrive(t, cid, func(drive *config.CatalogDrive) {
		drive.OwnerAccountCanonical = cid.String()
	})
	return cfg, loadStoredCatalogForTest(t)
}

func setupMissingAccountView(t *testing.T) (*config.Config, *config.Catalog) {
	t.Helper()

	cid := driveid.MustCanonicalID("personal:missing@example.com")
	seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
		account.DisplayName = accountViewLifecycleMissingUser
	})
	return config.DefaultConfig(), loadStoredCatalogForTest(t)
}

func setupInvalidAccountView(t *testing.T) (*config.Config, *config.Catalog) {
	t.Helper()

	cid := driveid.MustCanonicalID("personal:invalid@example.com")
	seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
		account.DisplayName = accountViewLifecycleInvalidUser
	})
	require.NoError(t, writeInvalidTokenFile(config.DriveTokenPath(cid)))
	return config.DefaultConfig(), loadStoredCatalogForTest(t)
}

func setupRejectedAccountView(t *testing.T) (*config.Config, *config.Catalog) {
	t.Helper()

	cid := driveid.MustCanonicalID("personal:rejected@example.com")
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_rejected@example.com.json")
	seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
		account.DisplayName = accountViewLifecycleRejectedUser
		account.AuthRequirementReason = authstate.ReasonSyncAuthRejected
	})
	return config.DefaultConfig(), loadStoredCatalogForTest(t)
}

func setupNoConfiguredDriveAccountView(t *testing.T) (*config.Config, *config.Catalog) {
	t.Helper()

	cid := driveid.MustCanonicalID("business:noconfig@contoso.com")
	writeTestTokenFile(t, config.DefaultDataDir(), "token_business_noconfig@contoso.com.json")
	seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
		account.DisplayName = accountViewLifecycleNoConfigUser
	})
	return config.DefaultConfig(), loadStoredCatalogForTest(t)
}

func loadStoredCatalogForTest(t *testing.T) *config.Catalog {
	t.Helper()

	stored, err := config.LoadCatalog()
	require.NoError(t, err)
	return stored
}
