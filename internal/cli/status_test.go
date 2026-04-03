package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	_ "modernc.org/sqlite"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
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
	assert.Contains(t, cmd.Short, "authentication health")
	assert.Contains(t, cmd.Long, "authentication health")
	assert.NotContains(t, cmd.Long, "token status")
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
		Issues:           2,
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
	assert.Equal(t, 2, ss.Issues)
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

	info := querySyncState("/nonexistent/path/state.db", logger)
	assert.Nil(t, info)
}

func TestQuerySyncState_EmptyDB(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	// Create a minimal DB with the required tables.
	createTestStateDB(t, dbPath)

	info := querySyncState(dbPath, logger)
	require.NotNil(t, info)
	assert.Empty(t, info.LastSyncTime)
	assert.Equal(t, 0, info.FileCount)
	assert.Equal(t, 0, info.Issues)
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

	info := querySyncState(dbPath, logger)
	require.NotNil(t, info)
	assert.Equal(t, "2026-03-02T10:30:00Z", info.LastSyncTime)
	assert.Equal(t, "1500", info.LastSyncDuration)
	assert.Empty(t, info.LastError)
	assert.Equal(t, 1, info.FileCount)
	assert.Equal(t, 1, info.Issues) // 1 conflict = 1 issue
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

	info := querySyncState(dbPath, logger)
	require.NotNil(t, info)
	assert.Equal(t, 2, info.PendingSync) // pending_download + download_failed
	assert.Equal(t, 1, info.Issues)      // 1 actionable failure
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

	info := querySyncState(dbPath, logger)
	require.NotNil(t, info)
	assert.Equal(t, 4, info.Issues)
}

func TestComputeSummary_Mixed(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			AuthState: authStateReady,
			Drives: []statusDrive{
				{State: driveStateReady, SyncState: &syncStateInfo{Issues: 3}},
				{State: driveStatePaused},
			},
		},
		{
			AuthState: authStateAuthenticationNeeded,
			Drives: []statusDrive{
				{State: driveStateReady},
				{State: driveStateReady, SyncState: &syncStateInfo{Issues: 1}},
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
					SyncState:   &syncStateInfo{FileCount: 10, Issues: 1},
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

// --- printStatusText ---

func TestPrintStatusText_NoDrives(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printStatusText(&buf, nil))

	output := buf.String()
	// Should still print summary line.
	assert.Contains(t, output, "Summary: 0 drives")
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
	require.NoError(t, printStatusText(&buf, accounts))

	output := buf.String()
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
	require.NoError(t, printStatusText(&buf, accounts))

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
	require.NoError(t, printStatusText(&buf, accounts))

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
	require.NoError(t, printStatusText(&buf, accounts))

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
	require.NoError(t, printStatusText(&buf, accounts))

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

func TestPrintSyncStateText_WithPendingAndIssues(t *testing.T) {
	t.Parallel()

	ss := &syncStateInfo{
		LastSyncTime: "2026-03-02T10:30:00Z",
		FileCount:    45,
		Issues:       0,
		PendingSync:  3,
		Retrying:     2,
	}

	var buf bytes.Buffer
	require.NoError(t, printSyncStateText(&buf, ss))

	output := buf.String()
	assert.Contains(t, output, "Pending:   3 items")
	assert.Contains(t, output, "Retrying:  2 items")
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

// createTestStateDB creates a minimal SQLite DB with tables matching the sync schema.
func createTestStateDB(t *testing.T, dbPath string) {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+dbPath)
	require.NoError(t, err)
	defer db.Close()

	// Create minimal tables for status queries.
	_, err = db.ExecContext(t.Context(), `
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
			detected_at INTEGER NOT NULL,
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
			manual_trial_requested_at INTEGER NOT NULL DEFAULT 0,
			last_error TEXT,
			http_status INTEGER,
			first_seen_at INTEGER NOT NULL,
			last_seen_at INTEGER NOT NULL,
			file_size INTEGER,
			local_hash TEXT,
			scope_key TEXT,
			PRIMARY KEY (path, drive_id)
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
	`)
	require.NoError(t, err)
}
