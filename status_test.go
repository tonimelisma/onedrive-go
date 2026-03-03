package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// mockMetaReader returns fixed display name and org name for testing.
type mockMetaReader struct {
	displayName string
	orgName     string
}

func (m *mockMetaReader) ReadMeta(_ string, _ []driveid.CanonicalID) (string, string) {
	return m.displayName, m.orgName
}

// mockTokenChecker returns a fixed token state for all accounts.
type mockTokenChecker struct {
	state string
}

func (m *mockTokenChecker) CheckToken(_ context.Context, _ string, _ []driveid.CanonicalID) string {
	return m.state
}

func TestDriveState_Ready(t *testing.T) {
	d := &config.Drive{}
	assert.Equal(t, "ready", driveState(d, tokenStateValid))
}

func TestDriveState_Paused(t *testing.T) {
	paused := true
	d := &config.Drive{Paused: &paused}
	assert.Equal(t, "paused", driveState(d, tokenStateValid))
}

func TestDriveState_NoToken(t *testing.T) {
	d := &config.Drive{}
	assert.Equal(t, "no token", driveState(d, tokenStateMissing))
}

func TestDriveState_PausedOverridesNoToken(t *testing.T) {
	// Paused takes priority over no token — the drive is intentionally paused.
	paused := true
	d := &config.Drive{Paused: &paused}
	assert.Equal(t, "paused", driveState(d, tokenStateMissing))
}

func TestGroupDrivesByAccount(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"):   {},
			driveid.MustCanonicalID("business:alice@example.com"):   {},
			driveid.MustCanonicalID("personal:bob@example.com"):     {},
			driveid.MustCanonicalID("business:charlie@example.com"): {},
		},
	}

	grouped, order := groupDrivesByAccount(cfg)

	// Order should be sorted alphabetically.
	assert.Len(t, order, 3)
	assert.Equal(t, "alice@example.com", order[0])
	assert.Equal(t, "bob@example.com", order[1])
	assert.Equal(t, "charlie@example.com", order[2])

	// alice has 2 drives.
	assert.Len(t, grouped["alice@example.com"], 2)
	assert.Len(t, grouped["bob@example.com"], 1)
	assert.Len(t, grouped["charlie@example.com"], 1)
}

func TestGroupDrivesByAccount_WithSharePoint(t *testing.T) {
	// With typed CanonicalID keys, SharePoint drives are grouped
	// under the same account as personal/business drives via .Email().
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("business:alice@contoso.com"):                    {},
			driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"):   {},
			driveid.MustCanonicalID("sharepoint:alice@contoso.com:engineering:Wiki"): {},
		},
	}

	grouped, order := groupDrivesByAccount(cfg)

	// All three drives belong to alice@contoso.com.
	assert.Len(t, order, 1)
	assert.Equal(t, "alice@contoso.com", order[0])
	assert.Len(t, grouped["alice@contoso.com"], 3)
}

func TestGroupDrivesByAccount_Empty(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{},
	}

	grouped, order := groupDrivesByAccount(cfg)

	assert.Empty(t, order)
	assert.Empty(t, grouped)
}

func TestNewStatusCmd_Structure(t *testing.T) {
	cmd := newStatusCmd()
	assert.Equal(t, "status", cmd.Name())
	assert.NotEmpty(t, cmd.Short)
	assert.NotNil(t, cmd.RunE)
}

// --- buildStatusAccountsWith tests (B-036) ---

func TestBuildStatusAccountsWith_SingleAccount(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"): {SyncDir: "~/OneDrive"},
		},
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockMetaReader{displayName: "Alice", orgName: ""},
		&mockTokenChecker{state: tokenStateValid},
	)

	require.Len(t, accounts, 1)
	acct := accounts[0]
	assert.Equal(t, "alice@example.com", acct.Email)
	assert.Equal(t, "personal", acct.DriveType)
	assert.Equal(t, tokenStateValid, acct.TokenState)
	assert.Equal(t, "Alice", acct.DisplayName)

	require.Len(t, acct.Drives, 1)
	assert.Equal(t, "~/OneDrive", acct.Drives[0].SyncDir)
	assert.Equal(t, driveStateReady, acct.Drives[0].State)
}

func TestBuildStatusAccountsWith_MultiAccountGrouping(t *testing.T) {
	paused := true
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"):   {SyncDir: "~/OneDrive"},
			driveid.MustCanonicalID("business:alice@example.com"):   {SyncDir: "~/Work"},
			driveid.MustCanonicalID("personal:bob@example.com"):     {SyncDir: "~/Bob", Paused: &paused},
			driveid.MustCanonicalID("business:charlie@example.com"): {SyncDir: "~/Charlie"},
		},
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockMetaReader{displayName: "", orgName: ""},
		&mockTokenChecker{state: tokenStateValid},
	)

	require.Len(t, accounts, 3)

	// Sorted alphabetically by email.
	assert.Equal(t, "alice@example.com", accounts[0].Email)
	assert.Len(t, accounts[0].Drives, 2)

	assert.Equal(t, "bob@example.com", accounts[1].Email)
	assert.Len(t, accounts[1].Drives, 1)
	assert.Equal(t, driveStatePaused, accounts[1].Drives[0].State)

	assert.Equal(t, "charlie@example.com", accounts[2].Email)
}

func TestBuildStatusAccountsWith_MissingToken(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"): {SyncDir: "~/OneDrive"},
		},
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockMetaReader{},
		&mockTokenChecker{state: tokenStateMissing},
	)

	require.Len(t, accounts, 1)
	assert.Equal(t, tokenStateMissing, accounts[0].TokenState)
	assert.Equal(t, driveStateNoToken, accounts[0].Drives[0].State)
}

func TestBuildStatusAccountsWith_ExpiredToken(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"): {SyncDir: "~/OneDrive"},
		},
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockMetaReader{},
		&mockTokenChecker{state: tokenStateExpired},
	)

	require.Len(t, accounts, 1)
	assert.Equal(t, tokenStateExpired, accounts[0].TokenState)
	// Expired token still shows "ready" — token state is shown separately.
	assert.Equal(t, driveStateReady, accounts[0].Drives[0].State)
}

func TestBuildStatusAccountsWith_EmptySyncDir(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"): {SyncDir: ""},
		},
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockMetaReader{},
		&mockTokenChecker{state: tokenStateValid},
	)

	require.Len(t, accounts, 1)
	assert.Equal(t, driveStateNeedsSetup, accounts[0].Drives[0].State)
}

func TestBuildStatusAccountsWith_SharePointGrouping(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("business:alice@contoso.com"):                    {SyncDir: "~/Work"},
			driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"):   {SyncDir: "~/Marketing"},
			driveid.MustCanonicalID("sharepoint:alice@contoso.com:engineering:Wiki"): {SyncDir: "~/Eng"},
		},
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockMetaReader{displayName: "Alice", orgName: "Contoso"},
		&mockTokenChecker{state: tokenStateValid},
	)

	require.Len(t, accounts, 1)
	acct := accounts[0]
	assert.Equal(t, "alice@contoso.com", acct.Email)
	assert.Equal(t, "business", acct.DriveType) // business preferred over sharepoint
	assert.Equal(t, "Contoso", acct.OrgName)
	assert.Len(t, acct.Drives, 3)
}

func TestBuildStatusAccountsWith_DisplayNameFromConfig(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"): {
				SyncDir:     "~/OneDrive",
				DisplayName: "My Home Drive",
			},
		},
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockMetaReader{},
		&mockTokenChecker{state: tokenStateValid},
	)

	require.Len(t, accounts, 1)
	assert.Equal(t, "My Home Drive", accounts[0].Drives[0].DisplayName)
}

func TestBuildStatusAccountsWith_PausedOverridesNoToken(t *testing.T) {
	paused := true
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"): {
				SyncDir: "~/OneDrive",
				Paused:  &paused,
			},
		},
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockMetaReader{},
		&mockTokenChecker{state: tokenStateMissing},
	)

	require.Len(t, accounts, 1)
	assert.Equal(t, driveStatePaused, accounts[0].Drives[0].State)
}

func TestBuildStatusAccountsWith_EmptyConfig(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{},
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockMetaReader{},
		&mockTokenChecker{state: tokenStateValid},
	)

	assert.Empty(t, accounts)
}
