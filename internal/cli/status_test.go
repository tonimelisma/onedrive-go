package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	_ "modernc.org/sqlite"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

// mockNameReader returns fixed display name and org name for testing.
type mockNameReader struct {
	displayName string
	orgName     string
}

func (m *mockNameReader) ReadAccountNames(_ string, _ []driveid.CanonicalID) (string, string) {
	return m.displayName, m.orgName
}

// mockSavedLoginChecker returns a fixed auth-health state for all accounts.
type mockSavedLoginChecker struct {
	state string
}

func (m *mockSavedLoginChecker) CheckAccountAuth(_ context.Context, _ string, _ []driveid.CanonicalID) accountAuthHealth {
	switch m.state {
	case savedLoginStateMissing:
		return accountAuthHealth{
			State:  authStateAuthenticationNeeded,
			Reason: authReasonMissingLogin,
			Action: authAction(authReasonMissingLogin),
		}
	case savedLoginStateInvalid:
		return accountAuthHealth{
			State:  authStateAuthenticationNeeded,
			Reason: authReasonInvalidSavedLogin,
			Action: authAction(authReasonInvalidSavedLogin),
		}
	default:
		return accountAuthHealth{State: authStateReady}
	}
}

// mockSyncStateQuerier returns a fixed sync state for all drives.
type mockSyncStateQuerier struct {
	state *syncStateInfo
}

func (m *mockSyncStateQuerier) QuerySyncState(_ driveid.CanonicalID) *syncStateInfo {
	return m.state
}

func TestDriveState_Ready(t *testing.T) {
	d := &config.Drive{}
	assert.Equal(t, "ready", driveState(d))
}

func TestDriveState_Paused(t *testing.T) {
	paused := true
	d := &config.Drive{Paused: &paused}
	assert.Equal(t, "paused", driveState(d))
}

func TestDriveState_PausedOverridesNoToken(t *testing.T) {
	// Paused remains an explicit operational state regardless of auth health.
	paused := true
	d := &config.Drive{Paused: &paused}
	assert.Equal(t, "paused", driveState(d))
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

// Validates: R-6.3.2
func TestNewStatusCmd_Structure(t *testing.T) {
	cmd := newStatusCmd()
	assert.Equal(t, "status", cmd.Name())
	assert.NotEmpty(t, cmd.Short)
	assert.NotNil(t, cmd.RunE)
	assert.Contains(t, cmd.Short, "sync status")
	assert.Contains(t, cmd.Long, "same per-drive sync-health contract")
	assert.Contains(t, cmd.Long, "--drive to filter")
	assert.Contains(t, cmd.Long, "--history")
	assert.Contains(t, cmd.Long, "--verbose")
}

// --- buildStatusAccountsWith tests (B-036) ---

func TestBuildStatusAccountsWith_SingleAccount(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"): {SyncDir: "~/OneDrive"},
		},
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockNameReader{displayName: "Alice", orgName: ""},
		&mockSavedLoginChecker{state: savedLoginStateUsable},
		&mockSyncStateQuerier{},
	)

	require.Len(t, accounts, 1)
	acct := accounts[0]
	assert.Equal(t, "alice@example.com", acct.Email)
	assert.Equal(t, "personal", acct.DriveType)
	assert.Equal(t, authStateReady, acct.AuthState)
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
		&mockNameReader{displayName: "", orgName: ""},
		&mockSavedLoginChecker{state: savedLoginStateUsable},
		&mockSyncStateQuerier{},
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

// Validates: R-2.10.47
func TestBuildStatusAccountsWith_AuthenticationRequiredStates(t *testing.T) {
	tests := []struct {
		name       string
		savedLogin string
		wantReason string
	}{
		{
			name:       "missing token",
			savedLogin: savedLoginStateMissing,
			wantReason: authReasonMissingLogin,
		},
		{
			name:       "invalid saved login",
			savedLogin: savedLoginStateInvalid,
			wantReason: authReasonInvalidSavedLogin,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				Drives: map[driveid.CanonicalID]config.Drive{
					driveid.MustCanonicalID("personal:alice@example.com"): {SyncDir: "~/OneDrive"},
				},
			}

			accounts := buildStatusAccountsWith(cfg,
				&mockNameReader{},
				&mockSavedLoginChecker{state: tc.savedLogin},
				&mockSyncStateQuerier{},
			)

			require.Len(t, accounts, 1)
			assert.Equal(t, authStateAuthenticationNeeded, accounts[0].AuthState)
			assert.Equal(t, tc.wantReason, accounts[0].AuthReason)
			assert.Equal(t, driveStateReady, accounts[0].Drives[0].State)
		})
	}
}

func TestBuildStatusAccountsWith_EmptySyncDir(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"): {SyncDir: ""},
		},
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockNameReader{},
		&mockSavedLoginChecker{state: savedLoginStateUsable},
		&mockSyncStateQuerier{},
	)

	require.Len(t, accounts, 1)
	// Empty sync_dir no longer overrides drive state — drive is still "ready".
	assert.Equal(t, driveStateReady, accounts[0].Drives[0].State)
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
		&mockNameReader{displayName: "Alice", orgName: "Contoso"},
		&mockSavedLoginChecker{state: savedLoginStateUsable},
		&mockSyncStateQuerier{},
	)

	require.Len(t, accounts, 1)
	acct := accounts[0]
	assert.Equal(t, "alice@contoso.com", acct.Email)
	assert.Equal(t, "business", acct.DriveType) // business preferred over sharepoint
	assert.Equal(t, "Contoso", acct.OrgName)
	assert.Len(t, acct.Drives, 3)
}

func TestReadAccountMeta_UsesProfileFieldOrder(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cid := driveid.MustCanonicalID("personal:alice@example.com")
	require.NoError(t, config.SaveAccountProfile(cid, &config.AccountProfile{
		DisplayName:    "Alice",
		OrgName:        "Contoso",
		UserID:         "u1",
		PrimaryDriveID: "d1",
	}))

	displayName, orgName := readAccountMeta("alice@example.com", []driveid.CanonicalID{cid}, slog.New(slog.DiscardHandler))
	assert.Equal(t, "Alice", displayName)
	assert.Equal(t, "Contoso", orgName)
}

func TestReadAccountMeta_FallsBackToTokenProbe(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cid := driveid.MustCanonicalID("personal:alice@example.com")
	require.NoError(t, config.SaveAccountProfile(cid, &config.AccountProfile{
		DisplayName:    "Alice",
		OrgName:        "Contoso",
		UserID:         "u1",
		PrimaryDriveID: "d1",
	}))

	tokenPath := config.DriveTokenPath(cid)
	require.NoError(t, os.MkdirAll(filepath.Dir(tokenPath), 0o700))
	require.NoError(t, os.WriteFile(tokenPath, []byte("{}"), 0o600))

	displayName, orgName := readAccountMeta("alice@example.com", nil, slog.New(slog.DiscardHandler))
	assert.Equal(t, "Alice", displayName)
	assert.Equal(t, "Contoso", orgName)
}

func TestInspectSavedLogin_MissingTokenUsesFallback(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	state := inspectSavedLogin(t.Context(), "missing@example.com", nil, slog.New(slog.DiscardHandler))
	assert.Equal(t, authReasonMissingLogin, state)
}

func TestInspectSavedLogin_ValidToken(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cid := driveid.MustCanonicalID("personal:alice@example.com")
	tokenPath := config.DriveTokenPath(cid)
	require.NoError(t, os.MkdirAll(filepath.Dir(tokenPath), 0o700))
	require.NoError(t, tokenfile.Save(tokenPath, &oauth2.Token{
		AccessToken:  "access",
		RefreshToken: "refresh",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
	}))

	state := inspectSavedLogin(t.Context(), "alice@example.com", []driveid.CanonicalID{cid}, slog.New(slog.DiscardHandler))
	assert.Empty(t, state)
}

func TestInspectSavedLogin_InvalidTokenFileReturnsInvalidSavedLogin(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cid := driveid.MustCanonicalID("personal:alice@example.com")
	tokenPath := config.DriveTokenPath(cid)
	require.NoError(t, os.MkdirAll(filepath.Dir(tokenPath), 0o700))
	require.NoError(t, os.WriteFile(tokenPath, []byte("{invalid-json"), 0o600))

	state := inspectSavedLogin(t.Context(), "alice@example.com", []driveid.CanonicalID{cid}, slog.New(slog.DiscardHandler))
	assert.Equal(t, authReasonInvalidSavedLogin, state)
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
		&mockNameReader{},
		&mockSavedLoginChecker{state: savedLoginStateUsable},
		&mockSyncStateQuerier{},
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
		&mockNameReader{},
		&mockSavedLoginChecker{state: savedLoginStateMissing},
		&mockSyncStateQuerier{},
	)

	require.Len(t, accounts, 1)
	assert.Equal(t, driveStatePaused, accounts[0].Drives[0].State)
	assert.Equal(t, authStateAuthenticationNeeded, accounts[0].AuthState)
}

func TestBuildStatusAccountsWith_EmptyConfig(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{},
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockNameReader{},
		&mockSavedLoginChecker{state: savedLoginStateUsable},
		&mockSyncStateQuerier{},
	)

	assert.Empty(t, accounts)
}

// --- 6.2b: Sync state and health summary tests ---

func TestBuildStatusAccountsWith_SyncStatePopulated(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"): {SyncDir: "~/OneDrive"},
		},
	}

	syncState := &syncStateInfo{
		LastSyncTime:     "2026-03-02T10:30:00Z",
		LastSyncDuration: "1500",
		FileCount:        45,
		IssueCount:       2,
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockNameReader{},
		&mockSavedLoginChecker{state: savedLoginStateUsable},
		&mockSyncStateQuerier{state: syncState},
	)

	require.Len(t, accounts, 1)
	require.Len(t, accounts[0].Drives, 1)
	require.NotNil(t, accounts[0].Drives[0].SyncState)

	ss := accounts[0].Drives[0].SyncState
	assert.Equal(t, "2026-03-02T10:30:00Z", ss.LastSyncTime)
	assert.Equal(t, "1500", ss.LastSyncDuration)
	assert.Equal(t, 45, ss.FileCount)
	assert.Equal(t, 2, ss.IssueCount)
}

func TestBuildStatusAccountsWith_NilSyncState(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"): {SyncDir: "~/OneDrive"},
		},
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockNameReader{},
		&mockSavedLoginChecker{state: savedLoginStateUsable},
		&mockSyncStateQuerier{state: nil},
	)

	require.Len(t, accounts, 1)
	assert.Nil(t, accounts[0].Drives[0].SyncState)
}

func TestQuerySyncState_NoDB(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)

	info := querySyncState("personal:missing@example.com", "/nonexistent/path/state.db", logger)
	require.NotNil(t, info)
	assert.Equal(t, stateStoreStatusMissing, info.StateStoreStatus)
}

func TestQuerySyncState_EmptyDB(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	// Create a minimal DB with the required tables.
	createTestStateDB(t, dbPath)

	info := querySyncState("personal:alice@example.com", dbPath, logger)
	require.NotNil(t, info)
	assert.Empty(t, info.LastSyncTime)
	assert.Equal(t, 0, info.FileCount)
	assert.Equal(t, 0, info.IssueCount)
	assert.Equal(t, stateStoreStatusHealthy, info.StateStoreStatus)
}

func TestQuerySyncState_WithMetadata(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	createTestStateDB(t, dbPath)

	// Insert sync metadata.
	db, err := sql.Open("sqlite", "file:"+dbPath)
	require.NoError(t, err)
	defer db.Close()

	ctx := t.Context()

	_, err = db.ExecContext(ctx, `INSERT INTO sync_metadata (key, value) VALUES
		('last_sync_time', '2026-03-02T10:30:00Z'),
		('last_sync_duration_ms', '1500'),
		('last_sync_error', '')`)
	require.NoError(t, err)

	// Insert a baseline entry.
	_, err = db.ExecContext(ctx, `INSERT INTO baseline (path, drive_id, item_id, parent_id, item_type, synced_at)
		VALUES ('/test.txt', 'd!123', 'item1', 'root', 'file', 0)`)
	require.NoError(t, err)

	// Insert an unresolved conflict.
	_, err = db.ExecContext(ctx, `INSERT INTO conflicts (id, path, drive_id, item_id, parent_id, item_type, conflict_type, resolution, detected_at)
		VALUES ('c1', '/conflict.txt', 'd!123', 'item2', 'root', 'file', 'edit_edit', 'unresolved', 0)`)
	require.NoError(t, err)

	require.NoError(t, db.Close())

	info := querySyncState("personal:alice@example.com", dbPath, logger)
	require.NotNil(t, info)
	assert.Equal(t, "2026-03-02T10:30:00Z", info.LastSyncTime)
	assert.Equal(t, "1500", info.LastSyncDuration)
	assert.Empty(t, info.LastError)
	assert.Equal(t, 1, info.FileCount)
	assert.Zero(t, info.IssueCount)
}

func TestQuerySyncState_PendingSyncAndIssues(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	createTestStateDB(t, dbPath)

	db, err := sql.Open("sqlite", "file:"+dbPath)
	require.NoError(t, err)

	ctx := t.Context()

	// Insert remote_state rows with mixed statuses.
	_, err = db.ExecContext(ctx, `INSERT INTO remote_state (path, drive_id, item_id, parent_id, item_type, sync_status, observed_at) VALUES
		('/a.txt', 'd!1', 'i1', 'root', 'file', 'synced', 0),
		('/b.txt', 'd!1', 'i2', 'root', 'file', 'pending_download', 0),
		('/c.txt', 'd!1', 'i3', 'root', 'file', 'download_failed', 0),
		('/d.txt', 'd!1', 'i4', 'root', 'file', 'deleted', 0),
		('/e.txt', 'd!1', 'i5', 'root', 'file', 'filtered', 0)`)
	require.NoError(t, err)

	// Insert sync_failures rows.
	_, err = db.ExecContext(ctx, `INSERT INTO sync_failures (path, drive_id, direction, action_type, failure_role, category, failure_count, first_seen_at, last_seen_at) VALUES
		('/x.txt', 'd!1', 'upload', 'upload', 'item', 'transient', 3, 0, 0),
		('/y.txt', 'd!1', 'upload', 'upload', 'item', 'transient', 5, 0, 0),
		('/z.txt', 'd!1', 'upload', 'upload', 'item', 'actionable', 1, 0, 0)`)
	require.NoError(t, err)

	require.NoError(t, db.Close())

	info := querySyncState("personal:alice@example.com", dbPath, logger)
	require.NotNil(t, info)
	assert.Equal(t, 2, info.PendingSync) // pending_download + download_failed
	assert.Equal(t, 1, info.IssueCount)  // 1 actionable failure
	assert.Equal(t, 2, info.Retrying)    // 2 transient with failure_count >= 3
}

// Validates: R-2.10.47, R-2.14.3
func TestQuerySyncState_CountsAuthAndRemoteBlockedScopesAsIssues(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	createTestStateDB(t, dbPath)

	db, err := sql.Open("sqlite", "file:"+dbPath)
	require.NoError(t, err)
	defer db.Close()

	ctx := t.Context()

	_, err = db.ExecContext(ctx, `INSERT INTO conflicts (id, path, drive_id, item_id, parent_id, item_type, conflict_type, resolution, detected_at)
		VALUES ('c1', '/conflict.txt', 'd!123', 'item2', 'root', 'file', 'edit_edit', 'unresolved', 0)`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO sync_failures
		(path, drive_id, direction, action_type, failure_role, category, issue_type, scope_key, failure_count, first_seen_at, last_seen_at)
		VALUES
		('/blocked/a.txt', 'd!1', 'upload', 'upload', 'held', 'transient', 'shared_folder_write_blocked', 'perm:remote:Shared/Docs', 1, 0, 0),
		('/blocked/b.txt', 'd!1', 'upload', 'upload', 'held', 'transient', 'shared_folder_write_blocked', 'perm:remote:Shared/Docs', 1, 0, 0),
		('/actionable.txt', 'd!1', 'upload', 'upload', 'item', 'actionable', 'invalid_filename', '', 1, 0, 0)`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO scope_blocks
		(scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, preserve_until, trial_count)
		VALUES ('auth:account', 'unauthorized', 'none', 1, 0, 0, 0, 0)`)
	require.NoError(t, err)

	info := querySyncState("personal:alice@example.com", dbPath, logger)
	require.NotNil(t, info)
	assert.Equal(t, 4, info.IssueCount)
}

// Validates: R-2.3.6, R-2.3.12
func TestQuerySyncState_DurableIntentCountsAndActionHints(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := syncstore.NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)
	defer func() {
		assert.NoError(t, store.Close(t.Context()))
	}()

	ctx := t.Context()
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO conflicts
			(id, drive_id, item_id, path, conflict_type, detected_at, resolution)
		VALUES
			('conflict-pending', 'd!1', 'item-1', '/pending.txt', 'edit_edit', 1, 'unresolved'),
			('conflict-resolving', 'd!1', 'item-2', '/resolving.txt', 'edit_edit', 2, 'unresolved'),
			('conflict-failed', 'd!1', 'item-3', '/failed.txt', 'edit_edit', 3, 'unresolved')`)
	require.NoError(t, err)

	require.NoError(t, store.UpsertHeldDeletes(ctx, []synctypes.HeldDeleteRecord{{
		DriveID:       driveid.New("d!1"),
		ActionType:    synctypes.ActionRemoteDelete,
		Path:          "/delete-me.txt",
		ItemID:        "item-delete",
		State:         synctypes.HeldDeleteStateHeld,
		HeldAt:        1,
		LastPlannedAt: 1,
	}}))
	require.NoError(t, store.ApproveHeldDeletes(ctx))

	queued, err := store.RequestConflictResolution(ctx, "conflict-pending", synctypes.ResolutionKeepLocal)
	require.NoError(t, err)
	assert.Equal(t, syncstore.ConflictRequestQueued, queued.Status)

	resolving, err := store.RequestConflictResolution(ctx, "conflict-resolving", synctypes.ResolutionKeepRemote)
	require.NoError(t, err)
	assert.Equal(t, syncstore.ConflictRequestQueued, resolving.Status)
	_, ok, err := store.ClaimConflictResolution(ctx, "conflict-resolving")
	require.NoError(t, err)
	require.True(t, ok)

	failed, err := store.RequestConflictResolution(ctx, "conflict-failed", synctypes.ResolutionKeepBoth)
	require.NoError(t, err)
	assert.Equal(t, syncstore.ConflictRequestQueued, failed.Status)
	_, ok, err = store.ClaimConflictResolution(ctx, "conflict-failed")
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, store.MarkConflictResolutionFailed(ctx, "conflict-failed", assert.AnError))

	info := querySyncState("personal:alice@example.com", dbPath, logger)
	require.NotNil(t, info)
	assert.Equal(t, 1, info.ApprovedDeletesWaiting)
	assert.Equal(t, 2, info.QueuedConflictRequests)
	assert.Equal(t, 1, info.ApplyingConflictRequests)
	assert.Contains(t, info.NextActions, "run `onedrive-go --drive personal:alice@example.com sync` or start `onedrive-go --drive personal:alice@example.com sync --watch` to execute approved deletes.")
	assert.Contains(t, info.NextActions, "run `onedrive-go --drive personal:alice@example.com sync` or start `onedrive-go --drive personal:alice@example.com sync --watch` to apply queued resolutions.")
	assert.Contains(t, info.NextActions, "wait for the active sync owner to finish, then run `onedrive-go --drive personal:alice@example.com status` again if needed.")
}

// Validates: R-6.10.5
func TestQuerySyncState_UsesReadOnlyProjectionHelper(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	store, err := syncstore.NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)

	_, err = store.DB().ExecContext(t.Context(), `INSERT INTO conflicts
		(id, drive_id, item_id, path, conflict_type, detected_at, resolution)
		VALUES ('c1', 'd!1', 'item-1', '/conflict.txt', 'edit_edit', 1, 'unresolved')`)
	require.NoError(t, err)

	walPath := dbPath + "-wal"
	shmPath := dbPath + "-shm"
	require.Eventually(t, func() bool {
		_, walErr := os.Stat(walPath)
		_, shmErr := os.Stat(shmPath)
		return walErr == nil && shmErr == nil
	}, time.Second, 10*time.Millisecond, "WAL sidecars were not created")

	require.NoError(t, os.Chmod(dbPath, 0o400))
	// #nosec G302 -- test intentionally makes the directory read-only to prove status stays on the read-only path.
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() {
		// #nosec G302 -- cleanup restores the tempdir so the writable store can close.
		assert.NoError(t, os.Chmod(dir, 0o700))
		assert.NoError(t, os.Chmod(dbPath, 0o600))
		assert.NoError(t, store.Close(context.Background()))
	})

	info := querySyncState("personal:alice@example.com", dbPath, logger)
	require.NotNil(t, info)
	assert.Zero(t, info.IssueCount)
}

// Validates: R-2.10.4, R-2.10.32
func TestQuerySyncState_PreservesIssueGroupScopeContext(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	createTestStateDB(t, dbPath)

	db, err := sql.Open("sqlite", "file:"+dbPath)
	require.NoError(t, err)
	defer db.Close()

	ctx := t.Context()

	_, err = db.ExecContext(ctx, `INSERT INTO shortcuts
		(item_id, remote_drive, remote_item, local_path, drive_type, observation, discovered_at)
		VALUES ('shortcut-1', 'remote-drive', 'remote-item', 'Shared/Team Docs', 'business', 'delta', 1)`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO sync_failures
		(path, drive_id, direction, action_type, failure_role, category, issue_type, scope_key, failure_count, first_seen_at, last_seen_at)
		VALUES
		('/invalid:name.txt', 'd!1', 'upload', 'upload', 'item', 'actionable', ?, '', 1, 0, 0),
		('/quota/a.txt', 'd!1', 'upload', 'upload', 'item', 'actionable', ?, ?, 1, 0, 0),
		('/quota/b.txt', 'd!1', 'upload', 'upload', 'item', 'actionable', ?, ?, 1, 0, 0)`,
		synctypes.IssueInvalidFilename,
		synctypes.IssueQuotaExceeded, synctypes.SKQuotaShortcut("remote-drive:remote-item").String(),
		synctypes.IssueQuotaExceeded, synctypes.SKQuotaShortcut("remote-drive:remote-item").String(),
	)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO scope_blocks
		(scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, preserve_until, trial_count)
		VALUES ('auth:account', ?, 'none', 1, 0, 0, 0, 0)`, synctypes.IssueUnauthorized)
	require.NoError(t, err)

	info := querySyncState("personal:alice@example.com", dbPath, logger)
	require.NotNil(t, info)
	invalidDescriptor := synctypes.DescribeSummary(synctypes.SummaryInvalidFilename)
	quotaDescriptor := synctypes.DescribeSummary(synctypes.SummaryQuotaExceeded)
	authDescriptor := synctypes.DescribeSummary(synctypes.SummaryAuthenticationRequired)
	assert.ElementsMatch(t, []failureGroupJSON{
		{
			SummaryKey: string(synctypes.SummaryInvalidFilename),
			IssueType:  string(synctypes.IssueInvalidFilename),
			Title:      invalidDescriptor.Title,
			Reason:     invalidDescriptor.Reason,
			Action:     invalidDescriptor.Action,
			Count:      1,
			Paths:      []string{"/invalid:name.txt"},
		},
		{
			SummaryKey: string(synctypes.SummaryQuotaExceeded),
			IssueType:  string(synctypes.IssueQuotaExceeded),
			Title:      quotaDescriptor.Title,
			Reason:     quotaDescriptor.Reason,
			Action:     quotaDescriptor.Action,
			Count:      2,
			ScopeKind:  "shortcut",
			Scope:      "Shared/Team Docs",
			Paths:      []string{"/quota/a.txt", "/quota/b.txt"},
		},
		{
			SummaryKey: string(synctypes.SummaryAuthenticationRequired),
			IssueType:  string(synctypes.IssueUnauthorized),
			Title:      authDescriptor.Title,
			Reason:     authDescriptor.Reason,
			Action:     authDescriptor.Action,
			Count:      1,
			ScopeKind:  "account",
			Scope:      "your OneDrive account authorization",
		},
	}, info.IssueGroups)
}

// Validates: R-2.10.32
func TestPrintStatusJSON_KeepsSameSummaryGroupsSeparatedByScope(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Email:     "alice@example.com",
			DriveType: "business",
			AuthState: authStateReady,
			Drives: []statusDrive{
				{
					CanonicalID: "business:alice@example.com",
					SyncDir:     "~/Work",
					State:       driveStateReady,
					SyncState: &syncStateInfo{
						FileCount:  10,
						IssueCount: 2,
						IssueGroups: []failureGroupJSON{
							{
								SummaryKey: string(synctypes.SummaryQuotaExceeded),
								Title:      "QUOTA EXCEEDED",
								Count:      1,
								ScopeKind:  "shortcut",
								Scope:      "Shared/Docs",
							},
							{
								SummaryKey: string(synctypes.SummaryQuotaExceeded),
								Title:      "QUOTA EXCEEDED",
								Count:      1,
								ScopeKind:  "shortcut",
								Scope:      "Shared/Design",
							},
						},
					},
				},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printStatusJSON(&buf, accounts))

	var result statusOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	require.Len(t, result.Accounts, 1)
	require.Len(t, result.Accounts[0].Drives, 1)
	require.Len(t, result.Accounts[0].Drives[0].SyncState.IssueGroups, 2)
	assert.Equal(t, "Shared/Docs", result.Accounts[0].Drives[0].SyncState.IssueGroups[0].Scope)
	assert.Equal(t, "Shared/Design", result.Accounts[0].Drives[0].SyncState.IssueGroups[1].Scope)
}

// Validates: R-2.10.32
func TestPrintSyncStateText_KeepsSameSummaryGroupsSeparatedByScope(t *testing.T) {
	t.Parallel()

	quotaDescriptor := synctypes.DescribeSummary(synctypes.SummaryQuotaExceeded)
	ss := &syncStateInfo{
		IssueCount: 2,
		IssueGroups: []failureGroupJSON{
			{
				SummaryKey: string(synctypes.SummaryQuotaExceeded),
				IssueType:  string(synctypes.IssueQuotaExceeded),
				Title:      quotaDescriptor.Title,
				Reason:     quotaDescriptor.Reason,
				Action:     quotaDescriptor.Action,
				Count:      1,
				ScopeKind:  "shortcut",
				Scope:      "Shared/Docs",
			},
			{
				SummaryKey: string(synctypes.SummaryQuotaExceeded),
				IssueType:  string(synctypes.IssueQuotaExceeded),
				Title:      quotaDescriptor.Title,
				Reason:     quotaDescriptor.Reason,
				Action:     quotaDescriptor.Action,
				Count:      1,
				ScopeKind:  "shortcut",
				Scope:      "Shared/Design",
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printSyncStateText(&buf, ss, false))
	output := buf.String()
	assert.Equal(t, 2, strings.Count(output, "QUOTA EXCEEDED (1 item)"))
	assert.Contains(t, output, "Scope: Shared/Docs")
	assert.Contains(t, output, "Scope: Shared/Design")
}

func TestComputeSummary_Mixed(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			AuthState: authStateReady,
			Drives: []statusDrive{
				{State: driveStateReady, SyncState: &syncStateInfo{IssueCount: 3}},
				{State: driveStatePaused},
			},
		},
		{
			AuthState: authStateAuthenticationNeeded,
			Drives: []statusDrive{
				{State: driveStateReady},
				{State: driveStateReady, SyncState: &syncStateInfo{IssueCount: 1}},
			},
		},
	}

	s := computeSummary(accounts)
	assert.Equal(t, 4, s.TotalDrives)
	assert.Equal(t, 3, s.Ready)
	assert.Equal(t, 1, s.Paused)
	assert.Equal(t, 1, s.AccountsRequiringAuth)
	assert.Equal(t, 4, s.TotalIssues)
}

func TestComputeSummary_Empty(t *testing.T) {
	t.Parallel()

	s := computeSummary(nil)
	assert.Equal(t, 0, s.TotalDrives)
	assert.Equal(t, 0, s.TotalIssues)
}

// --- printStatusJSON ---

func TestPrintStatusJSON_Empty(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := printStatusJSON(&buf, nil)
	require.NoError(t, err)

	var result statusOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	assert.Empty(t, result.Accounts)
}

func TestPrintStatusJSON_WithAccounts(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Email:     "alice@example.com",
			DriveType: "personal",
			AuthState: authStateReady,
			Drives: []statusDrive{
				{
					CanonicalID: "personal:alice@example.com",
					SyncDir:     "~/OneDrive",
					State:       driveStateReady,
					SyncState:   &syncStateInfo{FileCount: 10, IssueCount: 1},
				},
			},
		},
	}

	var buf bytes.Buffer
	err := printStatusJSON(&buf, accounts)
	require.NoError(t, err)

	var result statusOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	require.Len(t, result.Accounts, 1)
	assert.Equal(t, "alice@example.com", result.Accounts[0].Email)
	assert.Equal(t, 1, result.Summary.Ready)
}

// Validates: R-2.10.4
func TestPrintStatusJSON_WithIssueGroups(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Email:     "alice@example.com",
			DriveType: "personal",
			AuthState: authStateReady,
			Drives: []statusDrive{
				{
					CanonicalID: "personal:alice@example.com",
					SyncDir:     "~/OneDrive",
					State:       driveStateReady,
					SyncState: &syncStateInfo{
						FileCount:  10,
						IssueCount: 3,
						IssueGroups: []failureGroupJSON{
							{
								SummaryKey: string(synctypes.SummaryQuotaExceeded),
								Title:      "QUOTA EXCEEDED",
								Count:      1,
								ScopeKind:  "shortcut",
								Scope:      "Shared/Team Docs",
							},
							{
								SummaryKey: string(synctypes.SummaryInvalidFilename),
								Title:      "INVALID FILENAME",
								Count:      2,
								ScopeKind:  "file",
							},
						},
					},
				},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printStatusJSON(&buf, accounts))

	var result statusOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	require.Len(t, result.Accounts, 1)
	require.Len(t, result.Accounts[0].Drives, 1)
	require.NotNil(t, result.Accounts[0].Drives[0].SyncState)
	require.Len(t, result.Accounts[0].Drives[0].SyncState.IssueGroups, 2)
	assert.Equal(t, "shortcut", result.Accounts[0].Drives[0].SyncState.IssueGroups[0].ScopeKind)
	assert.Equal(t, "Shared/Team Docs", result.Accounts[0].Drives[0].SyncState.IssueGroups[0].Scope)
}

// --- printStatusText ---

func TestPrintStatusText_NoDrives(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printStatusText(&buf, nil, false))

	output := buf.String()
	assert.Equal(t, "Summary: 0 drives, 0 issues\n", output)
}

func TestPrintStatusText_WithDisplayNameAndOrg(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Email:       "alice@contoso.com",
			DisplayName: "Alice Smith",
			DriveType:   "business",
			OrgName:     "Contoso",
			AuthState:   authStateReady,
			Drives: []statusDrive{
				{
					CanonicalID: "business:alice@contoso.com",
					SyncDir:     "~/Work",
					State:       driveStateReady,
				},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printStatusText(&buf, accounts, false))

	output := buf.String()
	assert.True(t, strings.HasPrefix(output, "Summary: 1 drives (1 ready), 0 issues\n\n"))
	assert.Contains(t, output, "Alice Smith (alice@contoso.com)")
	assert.Contains(t, output, "Org:   Contoso")
	assert.Contains(t, output, "Auth:  ready")
	assert.Contains(t, output, "~/Work")
}

func TestPrintStatusText_WithAuthRequiredReasonAndAction(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Email:      "alice@example.com",
			DriveType:  "personal",
			AuthState:  authStateAuthenticationNeeded,
			AuthReason: authReasonSyncAuthRejected,
			AuthAction: authAction(authReasonSyncAuthRejected),
			Drives: []statusDrive{
				{
					CanonicalID: "personal:alice@example.com",
					SyncDir:     "~/OneDrive",
					State:       driveStateReady,
				},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printStatusText(&buf, accounts, false))

	output := buf.String()
	assert.Contains(t, output, "Auth:  authentication_required")
	assert.Contains(t, output, "The last sync attempt for this account was rejected by OneDrive.")
	assert.Contains(t, output, "whoami")
}

func TestPrintStatusText_SyncStateNever(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Email:     "bob@example.com",
			DriveType: "personal",
			AuthState: authStateReady,
			Drives: []statusDrive{
				{
					CanonicalID: "personal:bob@example.com",
					SyncDir:     "~/OneDrive",
					State:       driveStateReady,
					SyncState:   &syncStateInfo{},
				},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printStatusText(&buf, accounts, false))

	output := buf.String()
	assert.Contains(t, output, "Last sync: never")
}

func TestPrintStatusText_SyncStateWithError(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Email:     "bob@example.com",
			DriveType: "personal",
			AuthState: authStateReady,
			Drives: []statusDrive{
				{
					CanonicalID: "personal:bob@example.com",
					SyncDir:     "~/OneDrive",
					State:       driveStateReady,
					SyncState: &syncStateInfo{
						LastSyncTime: "2026-03-02T10:30:00Z",
						LastError:    "network timeout",
					},
				},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printStatusText(&buf, accounts, false))

	output := buf.String()
	assert.Contains(t, output, "Last error: network timeout")
}

func TestPrintStatusText_EmptySyncDir(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Email:     "bob@example.com",
			DriveType: "personal",
			AuthState: authStateReady,
			Drives: []statusDrive{
				{
					CanonicalID: "personal:bob@example.com",
					SyncDir:     "",
					State:       driveStateReady,
				},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printStatusText(&buf, accounts, false))

	output := buf.String()
	assert.Contains(t, output, syncDirNotSet)
}

func TestPrintSummaryText_AllStates(t *testing.T) {
	t.Parallel()

	s := statusSummary{
		TotalDrives:           4,
		Ready:                 3,
		Paused:                1,
		AccountsRequiringAuth: 1,
		TotalIssues:           3,
	}

	var buf bytes.Buffer
	require.NoError(t, printSummaryText(&buf, s))

	output := buf.String()
	assert.Contains(t, output, "4 drives")
	assert.Contains(t, output, "3 ready")
	assert.Contains(t, output, "1 paused")
	assert.Contains(t, output, "1 accounts requiring auth")
	assert.Contains(t, output, "3 issues")
}

func TestBuildSyncStateInfo_SamplesSectionsByDefault(t *testing.T) {
	t.Parallel()

	descriptor := synctypes.DescribeSummary(synctypes.SummaryInvalidFilename)
	paths := []string{"/one", "/two", "/three", "/four", "/five", "/six", "/seven"}
	deleteRows, conflicts, history := buildStatusSectionRows(paths, synctypes.ResolutionKeepRemote)

	info := buildSyncStateInfo(
		"personal:alice@example.com",
		&syncstore.DriveStatusSnapshot{
			IssueGroups: []syncstore.IssueGroupSnapshot{
				{
					SummaryKey:       synctypes.SummaryInvalidFilename,
					PrimaryIssueType: string(synctypes.IssueInvalidFilename),
					Count:            len(paths),
					Paths:            paths,
				},
			},
			DeleteSafety:    deleteRows,
			Conflicts:       conflicts,
			ConflictHistory: history,
		},
		driveStateStoreInfo{Status: stateStoreStatusHealthy},
		false,
		defaultVisiblePaths,
	)

	require.Len(t, info.IssueGroups, 1)
	assert.Equal(t, descriptor.Action, info.IssueGroups[0].Action)
	assert.Len(t, info.IssueGroups[0].Paths, defaultVisiblePaths)
	assert.Len(t, info.DeleteSafety, defaultVisiblePaths)
	assert.Len(t, info.Conflicts, defaultVisiblePaths)
	assert.Len(t, info.ConflictHistory, defaultVisiblePaths)
	assert.Equal(t, len(paths), info.DeleteSafetyTotal)
	assert.Equal(t, len(paths), info.ConflictsTotal)
	assert.Equal(t, len(paths), info.ConflictHistoryTotal)
	assert.Equal(t, defaultVisiblePaths, info.ExamplesLimit)
	assert.False(t, info.Verbose)
}

func TestBuildSyncStateInfo_VerboseExpandsSections(t *testing.T) {
	t.Parallel()

	paths := []string{"/one", "/two", "/three", "/four", "/five", "/six"}
	deleteRows, conflicts, history := buildStatusSectionRows(paths, synctypes.ResolutionKeepBoth)

	info := buildSyncStateInfo(
		"personal:alice@example.com",
		&syncstore.DriveStatusSnapshot{
			IssueGroups: []syncstore.IssueGroupSnapshot{
				{
					SummaryKey:       synctypes.SummaryInvalidFilename,
					PrimaryIssueType: string(synctypes.IssueInvalidFilename),
					Count:            len(paths),
					Paths:            paths,
				},
			},
			DeleteSafety:    deleteRows,
			Conflicts:       conflicts,
			ConflictHistory: history,
		},
		driveStateStoreInfo{Status: stateStoreStatusHealthy},
		true,
		defaultVisiblePaths,
	)

	assert.Len(t, info.IssueGroups[0].Paths, len(paths))
	assert.Len(t, info.DeleteSafety, len(paths))
	assert.Len(t, info.Conflicts, len(paths))
	assert.Len(t, info.ConflictHistory, len(paths))
	assert.True(t, info.Verbose)
}

func TestPrintSummaryText_WithPendingAndRetrying(t *testing.T) {
	t.Parallel()

	s := statusSummary{
		TotalDrives:      2,
		Ready:            2,
		TotalIssues:      1,
		TotalPendingSync: 5,
		TotalRetrying:    3,
	}

	var buf bytes.Buffer
	require.NoError(t, printSummaryText(&buf, s))

	output := buf.String()
	assert.Contains(t, output, "5 pending")
	assert.Contains(t, output, "3 retrying")
}

// Validates: R-2.10.4
func TestPrintSyncStateText_WithPendingAndIssues(t *testing.T) {
	t.Parallel()

	ss := &syncStateInfo{
		LastSyncTime: "2026-03-02T10:30:00Z",
		FileCount:    45,
		IssueCount:   0,
		PendingSync:  3,
		Retrying:     2,
	}

	var buf bytes.Buffer
	require.NoError(t, printSyncStateText(&buf, ss, false))

	output := buf.String()
	assert.Contains(t, output, "Pending:   3 items")
	assert.Contains(t, output, "Retrying:  2 items")
}

// Validates: R-2.10.4
func TestPrintSyncStateText_WithIssueGroups(t *testing.T) {
	t.Parallel()

	quotaDescriptor := synctypes.DescribeSummary(synctypes.SummaryQuotaExceeded)
	invalidDescriptor := synctypes.DescribeSummary(synctypes.SummaryInvalidFilename)
	authDescriptor := synctypes.DescribeSummary(synctypes.SummaryAuthenticationRequired)
	ss := &syncStateInfo{
		IssueCount: 3,
		Retrying:   2,
		IssueGroups: []failureGroupJSON{
			{
				SummaryKey: string(synctypes.SummaryQuotaExceeded),
				IssueType:  string(synctypes.IssueQuotaExceeded),
				Title:      quotaDescriptor.Title,
				Reason:     quotaDescriptor.Reason,
				Action:     quotaDescriptor.Action,
				Count:      1,
				ScopeKind:  "shortcut",
				Scope:      "Shared/Team Docs",
				Paths:      []string{"/quota/a.txt"},
			},
			{
				SummaryKey: string(synctypes.SummaryInvalidFilename),
				IssueType:  string(synctypes.IssueInvalidFilename),
				Title:      invalidDescriptor.Title,
				Reason:     invalidDescriptor.Reason,
				Action:     invalidDescriptor.Action,
				Count:      2,
				Paths:      []string{"/bad:name.txt", "/worse:name.txt"},
			},
			{
				SummaryKey: string(synctypes.SummaryAuthenticationRequired),
				IssueType:  string(synctypes.IssueUnauthorized),
				Title:      authDescriptor.Title,
				Reason:     authDescriptor.Reason,
				Action:     authDescriptor.Action,
				Count:      1,
				ScopeKind:  "account",
				Scope:      "your OneDrive account authorization",
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printSyncStateText(&buf, ss, false))
	requireGoldenText(t, "status_sync_state_with_issue_groups.golden", buf.String())
}

// Validates: R-2.3.6, R-2.3.12
func TestPrintSyncStateText_WithDurableIntentCountsAndHints(t *testing.T) {
	t.Parallel()

	ss := &syncStateInfo{
		StateStoreStatus:         stateStoreStatusHealthy,
		DeleteSafetyTotal:        1,
		ApprovedDeletesWaiting:   1,
		QueuedConflictRequests:   1,
		ApplyingConflictRequests: 1,
		DeleteSafety: []deleteSafetyJSON{
			{
				Path:       "/approved-delete.txt",
				State:      stateHeldDeleteApproved,
				ActionHint: "run `onedrive-go --drive personal:alice@example.com sync` or start `onedrive-go --drive personal:alice@example.com sync --watch` to execute approved deletes.",
			},
		},
		ConflictsTotal: 2,
		Conflicts: []statusConflictJSON{
			{
				Path:                "/queued-conflict.txt",
				ConflictType:        "edit_edit",
				State:               synctypes.ConflictStateQueued,
				RequestedResolution: synctypes.ResolutionKeepLocal,
				ActionHint:          "run `onedrive-go --drive personal:alice@example.com sync` or start `onedrive-go --drive personal:alice@example.com sync --watch` to apply queued resolutions.",
			},
			{
				Path:                "/applying-conflict.txt",
				ConflictType:        "edit_delete",
				State:               synctypes.ConflictStateApplying,
				RequestedResolution: synctypes.ResolutionKeepRemote,
				ActionHint:          "wait for the active sync owner to finish, then run `onedrive-go --drive personal:alice@example.com status` again if needed.",
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printSyncStateText(&buf, ss, false))

	output := buf.String()
	assert.Contains(t, output, "Approved deletes waiting: 1")
	assert.Contains(t, output, "/approved-delete.txt")
	assert.Contains(t, output, "Queued conflict resolutions: 1")
	assert.Contains(t, output, "Applying conflicts: 1")
	assert.Contains(t, output, "Next: run `onedrive-go --drive personal:alice@example.com sync` or start `onedrive-go --drive personal:alice@example.com sync --watch` to execute approved deletes.")
	assert.Contains(t, output, "Next: run `onedrive-go --drive personal:alice@example.com sync` or start `onedrive-go --drive personal:alice@example.com sync --watch` to apply queued resolutions.")
	assert.Contains(t, output, "Next: wait for the active sync owner to finish, then run `onedrive-go --drive personal:alice@example.com status` again if needed.")
}

func TestStatusService_Run_FilteredDriveText(t *testing.T) {
	cfgPath, cid := seedDriveStatusFixture(t)

	var out bytes.Buffer
	cc := newServiceContext(&out, cfgPath)
	cc.Flags.Drive = []string{cid.String()}

	require.NoError(t, newStatusService(cc).run(true))

	output := out.String()
	assert.Contains(t, output, "ISSUES")
	assert.Contains(t, output, "INVALID FILENAME")
	assert.Contains(t, output, "/bad:name.txt")
	assert.Contains(t, output, "DELETE SAFETY")
	assert.Contains(t, output, "Held deletes requiring approval: 1")
	assert.Contains(t, output, "/held-delete.txt")
	assert.Contains(t, output, "Next: run `onedrive-go --drive "+cid.String()+" resolve deletes`.")
	assert.Contains(t, output, "Approved deletes waiting for sync: 1")
	assert.Contains(t, output, "/approved-delete.txt")
	assert.Contains(t, output, "Next: run `onedrive-go --drive "+cid.String()+" sync` or start `onedrive-go --drive "+cid.String()+" sync --watch` to execute approved deletes.")
	assert.Contains(t, output, "CONFLICTS")
	assert.Contains(t, output, "/needs-choice.txt [edit_edit]")
	assert.Contains(t, output, "Decision: needed")
	assert.Contains(t, output, "Next: run `onedrive-go --drive "+cid.String()+" resolve local '/needs-choice.txt'` or replace `local` with `remote` or `both`.")
	assert.Contains(t, output, "/queued-conflict.txt [edit_edit]")
	assert.Contains(t, output, "Decision: keep_local (queued)")
	assert.Contains(t, output, "Last attempt: temporary upload failure")
	assert.Contains(t, output, "Next: run `onedrive-go --drive "+cid.String()+" sync` or start `onedrive-go --drive "+cid.String()+" sync --watch` to apply queued resolutions.")
	assert.Contains(t, output, "/applying-conflict.txt [edit_delete]")
	assert.Contains(t, output, "Decision: keep_remote (applying)")
	assert.Contains(t, output, "Next: wait for the active sync owner to finish, then run `onedrive-go --drive "+cid.String()+" status` again if needed.")
	assert.Contains(t, output, "CONFLICT HISTORY")
	assert.Contains(t, output, "/resolved-conflict.txt [create_create]")
	assert.Contains(t, output, "Resolved: keep_both by user")
	assert.Contains(t, output, "State DB:")
	assert.Contains(t, output, "healthy")
}

func TestStatusService_Run_FilteredDriveJSON(t *testing.T) {
	cfgPath, cid := seedDriveStatusFixture(t)

	var out bytes.Buffer
	cc := newServiceContext(&out, cfgPath)
	cc.Flags.Drive = []string{cid.String()}
	cc.Flags.JSON = true

	require.NoError(t, newStatusService(cc).run(true))

	var decoded statusOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	drive, syncState := requireSingleStatusDriveJSON(t, decoded, cid.String())
	assert.Equal(t, cid.String(), drive.CanonicalID)
	require.NotNil(t, syncState)
	assert.Equal(t, stateStoreStatusHealthy, syncState.StateStoreStatus)
	assert.Equal(t, defaultVisiblePaths, syncState.ExamplesLimit)
	assert.False(t, syncState.Verbose)
	assert.Len(t, syncState.IssueGroups, 1)
	assert.Equal(t, 2, syncState.DeleteSafetyTotal)
	assert.Len(t, syncState.DeleteSafety, 2)
	assert.Equal(t, 3, syncState.ConflictsTotal)
	assert.Len(t, syncState.Conflicts, 3)
	assert.Equal(t, 1, syncState.ConflictHistoryTotal)
	assert.Len(t, syncState.ConflictHistory, 1)
	assertStatusNextActions(t, syncState, cid.String())
	assertStatusDeleteSafetyActionHints(t, syncState, cid.String())
	assertStatusConflictActionHints(t, syncState, cid.String())
}

func TestStatusService_Run_AllDrivesJSON_FilteredDriveIsSubset(t *testing.T) {
	cfgPath, cids := seedMultiDriveStatusFixture(t, false)
	selected := cids[0]

	var allOut bytes.Buffer
	allCtx := newServiceContext(&allOut, cfgPath)
	allCtx.Flags.JSON = true
	require.NoError(t, newStatusService(allCtx).run(false))

	var filteredOut bytes.Buffer
	filteredCtx := newServiceContext(&filteredOut, cfgPath)
	filteredCtx.Flags.JSON = true
	filteredCtx.Flags.Drive = []string{selected.String()}
	require.NoError(t, newStatusService(filteredCtx).run(false))

	var allDecoded statusOutput
	require.NoError(t, json.Unmarshal(allOut.Bytes(), &allDecoded))
	var filteredDecoded statusOutput
	require.NoError(t, json.Unmarshal(filteredOut.Bytes(), &filteredDecoded))

	assert.Equal(t, 2, allDecoded.Summary.TotalDrives)
	assert.Equal(t, 1, filteredDecoded.Summary.TotalDrives)

	allDrive, allSyncState := findStatusDriveJSON(t, allDecoded, selected.String())
	filteredDrive, filteredSyncState := requireSingleStatusDriveJSON(t, filteredDecoded, selected.String())
	assert.Equal(t, allDrive, filteredDrive)
	require.NotNil(t, allSyncState)
	require.NotNil(t, filteredSyncState)
	assert.Equal(t, *allSyncState, *filteredSyncState)
}

func TestStatusService_Run_HistoryIncludesAllDisplayedDrives(t *testing.T) {
	cfgPath, cids := seedMultiDriveStatusFixture(t, true)

	var out bytes.Buffer
	cc := newServiceContext(&out, cfgPath)
	cc.Flags.JSON = true
	require.NoError(t, newStatusService(cc).run(true))

	var decoded statusOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	assert.Equal(t, len(cids), decoded.Summary.TotalDrives)

	for i := range cids {
		_, syncState := findStatusDriveJSON(t, decoded, cids[i].String())
		require.NotNil(t, syncState)
		require.Len(t, syncState.ConflictHistory, 1)
	}
}

func requireSingleStatusDriveJSON(
	t *testing.T,
	decoded statusOutput,
	canonicalID string,
) (statusDrive, *syncStateInfo) {
	t.Helper()

	drive, syncState := findStatusDriveJSON(t, decoded, canonicalID)
	require.Equal(t, 1, decoded.Summary.TotalDrives, "expected filtered status output")
	return drive, syncState
}

func findStatusDriveJSON(
	t *testing.T,
	decoded statusOutput,
	canonicalID string,
) (statusDrive, *syncStateInfo) {
	t.Helper()

	var (
		foundDrive statusDrive
		found      bool
	)
	for i := range decoded.Accounts {
		for j := range decoded.Accounts[i].Drives {
			drive := decoded.Accounts[i].Drives[j]
			if drive.CanonicalID == canonicalID {
				require.False(t, found, "expected exactly one drive in filtered status output")
				foundDrive = drive
				found = true
			}
		}
	}
	require.True(t, found, "expected drive %s in status output", canonicalID)
	return foundDrive, foundDrive.SyncState
}

func assertStatusNextActions(t *testing.T, decoded *syncStateInfo, canonicalID string) {
	t.Helper()

	assert.Contains(t, decoded.NextActions, "run `onedrive-go --drive "+canonicalID+" resolve deletes`.")
	assert.Contains(t, decoded.NextActions, "run `onedrive-go --drive "+canonicalID+" sync` or start `onedrive-go --drive "+canonicalID+" sync --watch` to execute approved deletes.")
	assert.Contains(t, decoded.NextActions, "run `onedrive-go --drive "+canonicalID+" resolve local '/needs-choice.txt'` or replace `local` with `remote` or `both`.")
	assert.Contains(t, decoded.NextActions, "run `onedrive-go --drive "+canonicalID+" sync` or start `onedrive-go --drive "+canonicalID+" sync --watch` to apply queued resolutions.")
	assert.Contains(t, decoded.NextActions, "wait for the active sync owner to finish, then run `onedrive-go --drive "+canonicalID+" status` again if needed.")
}

func assertStatusDeleteSafetyActionHints(t *testing.T, decoded *syncStateInfo, canonicalID string) {
	t.Helper()

	var (
		heldSeen     bool
		approvedSeen bool
	)
	for _, row := range decoded.DeleteSafety {
		switch row.Path {
		case "/held-delete.txt":
			heldSeen = true
			assert.Equal(t, "run `onedrive-go --drive "+canonicalID+" resolve deletes`.", row.ActionHint)
		case "/approved-delete.txt":
			approvedSeen = true
			assert.Equal(t, "run `onedrive-go --drive "+canonicalID+" sync` or start `onedrive-go --drive "+canonicalID+" sync --watch` to execute approved deletes.", row.ActionHint)
		}
	}
	assert.True(t, heldSeen)
	assert.True(t, approvedSeen)
}

func assertStatusConflictActionHints(t *testing.T, decoded *syncStateInfo, canonicalID string) {
	t.Helper()

	var (
		queuedFound    bool
		applyingFound  bool
		unresolvedSeen bool
	)
	for i := range decoded.Conflicts {
		conflict := decoded.Conflicts[i]
		switch conflict.Path {
		case "/queued-conflict.txt":
			queuedFound = true
			assert.Equal(t, synctypes.ConflictStateQueued, conflict.State)
			assert.Equal(t, synctypes.ResolutionKeepLocal, conflict.RequestedResolution)
			assert.Equal(t, "temporary upload failure", conflict.LastRequestError)
			assert.Equal(t, "run `onedrive-go --drive "+canonicalID+" sync` or start `onedrive-go --drive "+canonicalID+" sync --watch` to apply queued resolutions.", conflict.ActionHint)
		case "/applying-conflict.txt":
			applyingFound = true
			assert.Equal(t, synctypes.ConflictStateApplying, conflict.State)
			assert.Equal(t, synctypes.ResolutionKeepRemote, conflict.RequestedResolution)
			assert.Equal(t, "wait for the active sync owner to finish, then run `onedrive-go --drive "+canonicalID+" status` again if needed.", conflict.ActionHint)
		case "/needs-choice.txt":
			unresolvedSeen = true
			assert.Equal(t, synctypes.ConflictStateUnresolved, conflict.State)
			assert.Empty(t, conflict.RequestedResolution)
			assert.Equal(t, "run `onedrive-go --drive "+canonicalID+" resolve local '/needs-choice.txt'` or replace `local` with `remote` or `both`.", conflict.ActionHint)
		}
	}
	assert.True(t, queuedFound)
	assert.True(t, applyingFound)
	assert.True(t, unresolvedSeen)
}

func TestStatusService_Run_DamagedStateStoreSurfacesRecoverHint(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:damaged@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive"))
	require.NoError(t, os.MkdirAll(filepath.Dir(config.DriveStatePath(cid)), 0o700))
	require.NoError(t, os.WriteFile(config.DriveStatePath(cid), []byte("not a sqlite database"), 0o600))

	var out bytes.Buffer
	cc := newServiceContext(&out, cfgPath)
	cc.Flags.Drive = []string{cid.String()}
	cc.Flags.JSON = true

	require.NoError(t, newStatusService(cc).run(false))

	var decoded statusOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	_, syncState := requireSingleStatusDriveJSON(t, decoded, cid.String())
	require.NotNil(t, syncState)
	assert.Equal(t, stateStoreStatusDamaged, syncState.StateStoreStatus)
	assert.NotEmpty(t, syncState.StateStoreError)
	assert.Equal(t, recoverHintForDrive(cid.String()), syncState.StateStoreRecoveryHint)
	assert.Contains(t, syncState.NextActions, recoverHintForDrive(cid.String()))
}

func TestComputeSummary_AggregatesPendingAndRetrying(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Drives: []statusDrive{
				{State: driveStateReady, SyncState: &syncStateInfo{PendingSync: 3, Retrying: 1}},
				{State: driveStateReady, SyncState: &syncStateInfo{PendingSync: 2, Retrying: 4}},
			},
		},
	}

	s := computeSummary(accounts)
	assert.Equal(t, 5, s.TotalPendingSync)
	assert.Equal(t, 5, s.TotalRetrying)
}

func seedDriveStatusFixture(t *testing.T) (string, driveid.CanonicalID) {
	t.Helper()

	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:detailed@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive"))

	store, err := syncstore.NewSyncStore(t.Context(), config.DriveStatePath(cid), slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	ctx := t.Context()
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sync_metadata (key, value)
		VALUES
			('last_sync_time', '2026-04-03T10:30:00Z'),
			('last_sync_duration_ms', '1500');

		INSERT INTO baseline
			(drive_id, item_id, path, parent_id, item_type, local_hash, remote_hash,
			 local_size, remote_size, local_mtime, remote_mtime, synced_at, etag)
		VALUES
			('d!1', 'baseline-1', '/tracked.txt', 'root', 'file', 'lh', 'rh', 1, 1, 1, 1, 1, 'etag-1');

		INSERT INTO remote_state
			(drive_id, item_id, path, parent_id, item_type, hash, size, mtime, etag, sync_status,
			 filter_generation, filter_reason, observed_at)
		VALUES
			('d!1', 'remote-pending', '/pending.txt', 'root', 'file', 'rh-p', 5, 1, 'etag-p', 'pending_download', 0, '', 2),
			('d!1', 'remote-synced', '/synced.txt', 'root', 'file', 'rh-s', 5, 1, 'etag-s', 'synced', 0, '', 2);

		INSERT INTO sync_failures
			(path, drive_id, direction, action_type, category, failure_role, issue_type, item_id,
			 failure_count, next_retry_at, last_error, http_status, first_seen_at, last_seen_at,
			 file_size, local_hash, scope_key)
		VALUES
			('/bad:name.txt', 'd!1', 'upload', 'upload', 'actionable', 'item', 'invalid_filename',
			 'bad-item', 1, NULL, 'bad filename', NULL, 1, 2, NULL, NULL, '');

		INSERT INTO held_deletes
			(drive_id, action_type, path, item_id, state, held_at, approved_at, last_planned_at, last_error)
		VALUES
			('d!1', 'remote_delete', '/held-delete.txt', 'held-item', 'held', 1, NULL, 2, ''),
			('d!1', 'remote_delete', '/approved-delete.txt', 'approved-item', 'approved', 1, 3, 2, '');

		INSERT INTO conflicts
			(id, drive_id, item_id, path, conflict_type, detected_at, resolution,
			 local_hash, remote_hash, local_mtime, remote_mtime, resolved_at, resolved_by)
		VALUES
			('conflict-needs-choice', 'd!1', 'item-choice', '/needs-choice.txt', 'edit_edit', 10, 'unresolved', 'lh1', 'rh1', 1, 1, NULL, NULL),
			('conflict-queued', 'd!1', 'item-queued', '/queued-conflict.txt', 'edit_edit', 20, 'unresolved', 'lh2', 'rh2', 2, 2, NULL, NULL),
			('conflict-applying', 'd!1', 'item-applying', '/applying-conflict.txt', 'edit_delete', 30, 'unresolved', 'lh3', 'rh3', 3, 3, NULL, NULL),
			('conflict-resolved', 'd!1', 'item-resolved', '/resolved-conflict.txt', 'create_create', 40, 'keep_both', 'lh4', 'rh4', 4, 4, 50, 'user');`)
	require.NoError(t, err)

	result, err := store.RequestConflictResolution(ctx, "conflict-queued", synctypes.ResolutionKeepLocal)
	require.NoError(t, err)
	assert.Equal(t, syncstore.ConflictRequestQueued, result.Status)
	_, ok, err := store.ClaimConflictResolution(ctx, "conflict-queued")
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, store.MarkConflictResolutionFailed(ctx, "conflict-queued", errors.New("temporary upload failure")))

	result, err = store.RequestConflictResolution(ctx, "conflict-applying", synctypes.ResolutionKeepRemote)
	require.NoError(t, err)
	assert.Equal(t, syncstore.ConflictRequestQueued, result.Status)
	_, ok, err = store.ClaimConflictResolution(ctx, "conflict-applying")
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, store.Close(context.Background()))

	return cfgPath, cid
}

func seedMultiDriveStatusFixture(t *testing.T, withHistory bool) (string, []driveid.CanonicalID) {
	t.Helper()

	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cids := []driveid.CanonicalID{
		driveid.MustCanonicalID("personal:first@example.com"),
		driveid.MustCanonicalID("business:second@example.com"),
	}

	for i := range cids {
		require.NoError(t, config.AppendDriveSection(cfgPath, cids[i], filepath.Join("~", fmt.Sprintf("Drive%d", i+1))))
		store, err := syncstore.NewSyncStore(t.Context(), config.DriveStatePath(cids[i]), slog.New(slog.DiscardHandler))
		require.NoError(t, err)

		_, err = store.DB().ExecContext(t.Context(), `
			INSERT INTO sync_metadata (key, value)
			VALUES ('last_sync_time', ?);

			INSERT INTO baseline
				(drive_id, item_id, path, parent_id, item_type, local_hash, remote_hash,
				 local_size, remote_size, local_mtime, remote_mtime, synced_at, etag)
			VALUES
				('d!1', ?, '/tracked.txt', 'root', 'file', 'lh', 'rh', 1, 1, 1, 1, 1, 'etag-1');`,
			fmt.Sprintf("2026-04-0%dT10:30:00Z", i+1),
			fmt.Sprintf("baseline-%d", i+1),
		)
		require.NoError(t, err)

		if withHistory {
			_, err = store.DB().ExecContext(t.Context(), `
				INSERT INTO conflicts
					(id, drive_id, item_id, path, conflict_type, detected_at, resolution, resolved_at, resolved_by)
				VALUES
					(?, 'd!1', ?, ?, 'edit_edit', 10, 'keep_remote', 20, 'user')`,
				fmt.Sprintf("resolved-%d", i+1),
				fmt.Sprintf("item-%d", i+1),
				fmt.Sprintf("/resolved-%d.txt", i+1),
			)
			require.NoError(t, err)
		}

		require.NoError(t, store.Close(context.Background()))
	}

	return cfgPath, cids
}

func buildStatusSectionRows(
	paths []string,
	resolution string,
) ([]syncstore.DeleteSafetySnapshot, []syncstore.ConflictStatusSnapshot, []syncstore.ConflictHistorySnapshot) {
	deleteRows := make([]syncstore.DeleteSafetySnapshot, 0, len(paths))
	conflicts := make([]syncstore.ConflictStatusSnapshot, 0, len(paths))
	history := make([]syncstore.ConflictHistorySnapshot, 0, len(paths))
	for i := range paths {
		deleteRows = append(deleteRows, syncstore.DeleteSafetySnapshot{
			Path:       paths[i],
			State:      stateHeldDeleteHeld,
			LastSeenAt: int64(i + 1),
		})
		conflicts = append(conflicts, syncstore.ConflictStatusSnapshot{
			ID:           fmt.Sprintf("conflict-%d", i),
			Path:         paths[i],
			ConflictType: "edit_edit",
			DetectedAt:   int64(i + 1),
		})
		history = append(history, syncstore.ConflictHistorySnapshot{
			ID:           fmt.Sprintf("resolved-%d", i),
			Path:         paths[i],
			ConflictType: "edit_edit",
			DetectedAt:   int64(i + 1),
			Resolution:   resolution,
			ResolvedAt:   int64(i + 10),
		})
	}

	return deleteRows, conflicts, history
}

// createTestStateDB creates a minimal SQLite DB with tables matching the sync schema.
func createTestStateDB(t *testing.T, dbPath string) {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+dbPath)
	require.NoError(t, err)
	defer db.Close()

	_, err = db.ExecContext(t.Context(), statusTestStateSchema)
	require.NoError(t, err)
}

const statusTestStateSchema = `
		CREATE TABLE IF NOT EXISTS baseline (
			path TEXT PRIMARY KEY,
			drive_id TEXT NOT NULL,
			item_id TEXT NOT NULL,
			parent_id TEXT NOT NULL,
			item_type TEXT NOT NULL,
			size INTEGER,
			remote_hash TEXT,
			local_hash TEXT,
			mtime INTEGER,
			remote_mtime INTEGER,
			synced_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS conflicts (
			id TEXT PRIMARY KEY,
			path TEXT NOT NULL,
			drive_id TEXT NOT NULL,
			item_id TEXT NOT NULL,
			parent_id TEXT NOT NULL,
			item_type TEXT NOT NULL,
			conflict_type TEXT NOT NULL,
			resolution TEXT NOT NULL DEFAULT 'unresolved',
			state TEXT NOT NULL DEFAULT 'unresolved',
			requested_resolution TEXT,
			requested_at INTEGER,
			resolving_at INTEGER,
			resolution_error TEXT,
			detected_at INTEGER NOT NULL,
			local_hash TEXT,
			local_mtime INTEGER,
			resolved_at INTEGER,
			resolved_by TEXT,
			size INTEGER,
			remote_hash TEXT,
			mtime INTEGER,
			remote_mtime INTEGER
		);
		CREATE TABLE IF NOT EXISTS sync_metadata (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS remote_state (
			path TEXT PRIMARY KEY,
			drive_id TEXT NOT NULL,
			item_id TEXT NOT NULL,
			parent_id TEXT NOT NULL,
			item_type TEXT NOT NULL,
			sync_status TEXT NOT NULL DEFAULT 'synced',
			size INTEGER,
			hash TEXT,
			mtime INTEGER,
			etag TEXT,
			observed_at INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE IF NOT EXISTS sync_failures (
			path TEXT NOT NULL,
			drive_id TEXT NOT NULL,
			direction TEXT NOT NULL CHECK(direction IN ('download', 'upload', 'delete')),
			action_type TEXT NOT NULL CHECK(action_type IN ('download', 'upload', 'local_delete', 'remote_delete', 'local_move', 'remote_move', 'folder_create', 'conflict', 'update_synced', 'cleanup')),
			failure_role TEXT NOT NULL DEFAULT 'item' CHECK(failure_role IN ('item', 'held', 'boundary')),
			category TEXT NOT NULL CHECK(category IN ('transient', 'actionable')),
			issue_type TEXT,
			item_id TEXT,
			failure_count INTEGER NOT NULL DEFAULT 0,
			next_retry_at INTEGER,
			last_error TEXT,
			http_status INTEGER,
			first_seen_at INTEGER NOT NULL,
			last_seen_at INTEGER NOT NULL,
			file_size INTEGER,
			local_hash TEXT,
			scope_key TEXT,
			PRIMARY KEY (path, drive_id)
		);
		CREATE TABLE IF NOT EXISTS held_deletes (
			drive_id TEXT NOT NULL,
			action_type TEXT NOT NULL,
			path TEXT NOT NULL,
			item_id TEXT NOT NULL CHECK(item_id <> ''),
			state TEXT NOT NULL,
			held_at INTEGER NOT NULL,
			approved_at INTEGER,
			last_planned_at INTEGER NOT NULL,
			last_error TEXT,
			PRIMARY KEY (drive_id, action_type, path, item_id)
		);
		CREATE TABLE IF NOT EXISTS scope_blocks (
			scope_key TEXT PRIMARY KEY,
			issue_type TEXT NOT NULL,
			timing_source TEXT NOT NULL,
			blocked_at INTEGER NOT NULL,
			trial_interval INTEGER NOT NULL,
			next_trial_at INTEGER NOT NULL,
			preserve_until INTEGER NOT NULL,
			trial_count INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS shortcuts (
			item_id TEXT PRIMARY KEY,
			remote_drive TEXT NOT NULL,
			remote_item TEXT NOT NULL,
			local_path TEXT NOT NULL,
			drive_type TEXT NOT NULL DEFAULT '',
			observation TEXT NOT NULL DEFAULT 'unknown',
			read_only INTEGER NOT NULL DEFAULT 0,
			discovered_at INTEGER NOT NULL
		);
	`
