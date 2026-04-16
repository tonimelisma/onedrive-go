package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
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

	_, err = db.ExecContext(ctx, `UPDATE run_status
		SET last_completed_at = ?, last_duration_ms = 1500, last_succeeded_count = 0, last_failed_count = 0, last_error = ''
		WHERE singleton_id = 1`,
		time.Date(2026, 3, 2, 10, 30, 0, 0, time.UTC).UnixNano(),
	)
	require.NoError(t, err)

	// Insert a baseline entry.
	_, err = db.ExecContext(ctx, `INSERT INTO baseline (path, item_id, parent_id, item_type)
		VALUES ('/test.txt', 'item1', 'root', 'file')`)
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

func TestQuerySyncState_RemoteDriftAndIssues(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	createTestStateDB(t, dbPath)

	db, err := sql.Open("sqlite", "file:"+dbPath)
	require.NoError(t, err)

	ctx := t.Context()

	// Insert remote_state rows with mixed drift shapes.
	_, err = db.ExecContext(ctx, `INSERT INTO remote_state (path, item_id, parent_id, item_type) VALUES
		('/a.txt', 'i1', 'root', 'file'),
		('/b.txt', 'i2', 'root', 'file'),
		('/c.txt', 'i3', 'root', 'file'),
		('/e.txt', 'i5', 'root', 'file')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO baseline (item_id, path, parent_id, item_type, remote_hash, remote_mtime) VALUES
		('i1', '/a.txt', 'root', 'file', '', 0),
		('i4', '/d.txt', 'root', 'file', '', 0)`)
	require.NoError(t, err)

	// Insert sync_failures rows.
	_, err = db.ExecContext(ctx, `INSERT INTO sync_failures (path, direction, action_type, failure_role, category, failure_count, first_seen_at, last_seen_at) VALUES
		('/x.txt', 'upload', 'upload', 'item', 'transient', 3, 0, 0),
		('/y.txt', 'upload', 'upload', 'item', 'transient', 5, 0, 0),
		('/z.txt', 'upload', 'upload', 'item', 'actionable', 1, 0, 0)`)
	require.NoError(t, err)

	require.NoError(t, db.Close())

	info := querySyncState("personal:alice@example.com", dbPath, logger)
	require.NotNil(t, info)
	assert.Equal(t, 4, info.RemoteDrift) // three remote-only creates plus one baseline row missing on remote
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

	_, err = db.ExecContext(ctx, `INSERT INTO sync_failures
		(path, direction, action_type, failure_role, category, issue_type, scope_key, failure_count, first_seen_at, last_seen_at)
		VALUES
		('/blocked/a.txt', 'upload', 'upload', 'held', 'transient', 'remote_write_denied', 'perm:remote-write:Shared/Docs', 1, 0, 0),
		('/blocked/b.txt', 'upload', 'upload', 'held', 'transient', 'remote_write_denied', 'perm:remote-write:Shared/Docs', 1, 0, 0),
		('/actionable.txt', 'upload', 'upload', 'item', 'actionable', 'invalid_filename', '', 1, 0, 0)`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO scope_blocks
		(scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, preserve_until, trial_count)
		VALUES ('auth:account', 'unauthorized', 'none', 1, 0, 0, 0, 0)`)
	require.NoError(t, err)

	info := querySyncState("personal:alice@example.com", dbPath, logger)
	require.NotNil(t, info)
	assert.Equal(t, 4, info.IssueCount)
}

// Validates: R-6.10.5
func TestQuerySyncState_UsesReadOnlyProjectionHelper(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	store, err := syncengine.NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)

	_, err = store.DB().ExecContext(t.Context(), `INSERT INTO baseline
		(path, item_id, parent_id, item_type)
		VALUES ('/tracked.txt', 'item-1', 'root', 'file')`)
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

	_, err = db.ExecContext(ctx, `INSERT INTO sync_failures
		(path, direction, action_type, failure_role, category, issue_type, scope_key, failure_count, first_seen_at, last_seen_at)
		VALUES
		('/invalid:name.txt', 'upload', 'upload', 'item', 'actionable', ?, '', 1, 0, 0),
		('/blocked/a.txt', 'upload', 'upload', 'held', 'transient', ?, ?, 1, 0, 0),
		('/blocked/b.txt', 'upload', 'upload', 'held', 'transient', ?, ?, 1, 0, 0)`,
		syncengine.IssueInvalidFilename,
		syncengine.IssueSharedFolderBlocked, syncengine.SKPermRemote("Shared/Team Docs").String(),
		syncengine.IssueSharedFolderBlocked, syncengine.SKPermRemote("Shared/Team Docs").String(),
	)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO scope_blocks
		(scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, preserve_until, trial_count)
		VALUES ('auth:account', ?, 'none', 1, 0, 0, 0, 0)`, syncengine.IssueUnauthorized)
	require.NoError(t, err)

	info := querySyncState("personal:alice@example.com", dbPath, logger)
	require.NotNil(t, info)
	invalidDescriptor := describeStatusSummary(syncengine.SummaryInvalidFilename)
	sharedDescriptor := describeStatusSummary(syncengine.SummarySharedFolderWritesBlocked)
	authDescriptor := describeStatusSummary(syncengine.SummaryAuthenticationRequired)
	assert.ElementsMatch(t, []failureGroupJSON{
		{
			SummaryKey: string(syncengine.SummaryInvalidFilename),
			IssueType:  string(syncengine.IssueInvalidFilename),
			Title:      invalidDescriptor.Title,
			Reason:     invalidDescriptor.Reason,
			Action:     invalidDescriptor.Action,
			Count:      1,
			Paths:      []string{"/invalid:name.txt"},
		},
		{
			SummaryKey: string(syncengine.SummarySharedFolderWritesBlocked),
			IssueType:  string(syncengine.IssueSharedFolderBlocked),
			Title:      sharedDescriptor.Title,
			Reason:     sharedDescriptor.Reason,
			Action:     sharedDescriptor.Action,
			Count:      2,
			ScopeKind:  "directory",
			Scope:      "Shared/Team Docs",
			Paths:      []string{"/blocked/a.txt", "/blocked/b.txt"},
		},
		{
			SummaryKey: string(syncengine.SummaryAuthenticationRequired),
			IssueType:  string(syncengine.IssueUnauthorized),
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
								SummaryKey: string(syncengine.SummaryQuotaExceeded),
								Title:      "QUOTA EXCEEDED",
								Count:      1,
								ScopeKind:  "drive",
								Scope:      "Shared/Docs",
							},
							{
								SummaryKey: string(syncengine.SummaryQuotaExceeded),
								Title:      "QUOTA EXCEEDED",
								Count:      1,
								ScopeKind:  "drive",
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

	quotaDescriptor := describeStatusSummary(syncengine.SummaryQuotaExceeded)
	ss := &syncStateInfo{
		IssueCount: 2,
		IssueGroups: []failureGroupJSON{
			{
				SummaryKey: string(syncengine.SummaryQuotaExceeded),
				IssueType:  string(syncengine.IssueQuotaExceeded),
				Title:      quotaDescriptor.Title,
				Reason:     quotaDescriptor.Reason,
				Action:     quotaDescriptor.Action,
				Count:      1,
				ScopeKind:  "drive",
				Scope:      "Shared/Docs",
			},
			{
				SummaryKey: string(syncengine.SummaryQuotaExceeded),
				IssueType:  string(syncengine.IssueQuotaExceeded),
				Title:      quotaDescriptor.Title,
				Reason:     quotaDescriptor.Reason,
				Action:     quotaDescriptor.Action,
				Count:      1,
				ScopeKind:  "drive",
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
								SummaryKey: string(syncengine.SummaryQuotaExceeded),
								Title:      "QUOTA EXCEEDED",
								Count:      1,
								ScopeKind:  "drive",
								Scope:      "Shared/Team Docs",
							},
							{
								SummaryKey: string(syncengine.SummaryInvalidFilename),
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
	assert.Equal(t, "drive", result.Accounts[0].Drives[0].SyncState.IssueGroups[0].ScopeKind)
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

func TestPrintSummaryText_WithPendingAndRetrying(t *testing.T) {
	t.Parallel()

	s := statusSummary{
		TotalDrives:      2,
		Ready:            2,
		TotalIssues:      1,
		TotalRemoteDrift: 5,
		TotalRetrying:    3,
	}

	var buf bytes.Buffer
	require.NoError(t, printSummaryText(&buf, s))

	output := buf.String()
	assert.Contains(t, output, "5 remote drift")
	assert.Contains(t, output, "3 retrying")
}

// Validates: R-2.10.4
func TestPrintSyncStateText_WithPendingAndIssues(t *testing.T) {
	t.Parallel()

	ss := &syncStateInfo{
		LastSyncTime: "2026-03-02T10:30:00Z",
		FileCount:    45,
		IssueCount:   0,
		RemoteDrift:  3,
		Retrying:     2,
	}

	var buf bytes.Buffer
	require.NoError(t, printSyncStateText(&buf, ss, false))

	output := buf.String()
	assert.Contains(t, output, "Remote drift: 3 items")
	assert.Contains(t, output, "Retrying:  2 items")
}

// Validates: R-2.10.4
func TestPrintSyncStateText_WithIssueGroups(t *testing.T) {
	t.Parallel()

	quotaDescriptor := describeStatusSummary(syncengine.SummaryQuotaExceeded)
	invalidDescriptor := describeStatusSummary(syncengine.SummaryInvalidFilename)
	authDescriptor := describeStatusSummary(syncengine.SummaryAuthenticationRequired)
	ss := &syncStateInfo{
		IssueCount: 3,
		Retrying:   2,
		IssueGroups: []failureGroupJSON{
			{
				SummaryKey: string(syncengine.SummaryQuotaExceeded),
				IssueType:  string(syncengine.IssueQuotaExceeded),
				Title:      quotaDescriptor.Title,
				Reason:     quotaDescriptor.Reason,
				Action:     quotaDescriptor.Action,
				Count:      1,
				ScopeKind:  "drive",
				Scope:      "Shared/Team Docs",
				Paths:      []string{"/quota/a.txt"},
			},
			{
				SummaryKey: string(syncengine.SummaryInvalidFilename),
				IssueType:  string(syncengine.IssueInvalidFilename),
				Title:      invalidDescriptor.Title,
				Reason:     invalidDescriptor.Reason,
				Action:     invalidDescriptor.Action,
				Count:      2,
				Paths:      []string{"/bad:name.txt", "/worse:name.txt"},
			},
			{
				SummaryKey: string(syncengine.SummaryAuthenticationRequired),
				IssueType:  string(syncengine.IssueUnauthorized),
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

func TestStatusCommand_DamagedStateStoreSurfacesRecoverHint(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:damaged@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive"))
	require.NoError(t, os.MkdirAll(filepath.Dir(config.DriveStatePath(cid)), 0o700))
	require.NoError(t, os.WriteFile(config.DriveStatePath(cid), []byte("not a sqlite database"), 0o600))

	var out bytes.Buffer
	cc := newCommandContext(&out, cfgPath)
	cc.Flags.Drive = []string{cid.String()}
	cc.Flags.JSON = true

	require.NoError(t, runStatusCommand(cc, false))

	var decoded statusOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	_, syncState := requireSingleStatusDriveJSON(t, decoded, cid.String())
	require.NotNil(t, syncState)
	assert.Equal(t, stateStoreStatusDamaged, syncState.StateStoreStatus)
	assert.NotEmpty(t, syncState.StateStoreError)
	assert.Equal(t, recoverHintForDrive(cid.String()), syncState.StateStoreRecoveryHint)
}

func TestComputeSummary_AggregatesPendingAndRetrying(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Drives: []statusDrive{
				{State: driveStateReady, SyncState: &syncStateInfo{RemoteDrift: 3, Retrying: 1}},
				{State: driveStateReady, SyncState: &syncStateInfo{RemoteDrift: 2, Retrying: 4}},
			},
		},
	}

	s := computeSummary(accounts)
	assert.Equal(t, 5, s.TotalRemoteDrift)
	assert.Equal(t, 5, s.TotalRetrying)
}

// createTestStateDB creates a minimal SQLite DB with tables matching the sync schema.
func createTestStateDB(t *testing.T, dbPath string) {
	t.Helper()

	store, err := syncengine.NewSyncStore(t.Context(), dbPath, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	require.NoError(t, store.Close(t.Context()))
}
