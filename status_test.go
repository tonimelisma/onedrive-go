package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

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

// mockSyncStateQuerier returns a fixed sync state for all drives.
type mockSyncStateQuerier struct {
	state *syncStateInfo
}

func (m *mockSyncStateQuerier) QuerySyncState(_ driveid.CanonicalID) *syncStateInfo {
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
		&mockSyncStateQuerier{},
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

func TestBuildStatusAccountsWith_MissingToken(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"): {SyncDir: "~/OneDrive"},
		},
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockMetaReader{},
		&mockTokenChecker{state: tokenStateMissing},
		&mockSyncStateQuerier{},
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
		&mockSyncStateQuerier{},
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
		&mockSyncStateQuerier{},
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
		&mockSyncStateQuerier{},
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
		&mockMetaReader{},
		&mockTokenChecker{state: tokenStateMissing},
		&mockSyncStateQuerier{},
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
		Conflicts:        2,
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockMetaReader{},
		&mockTokenChecker{state: tokenStateValid},
		&mockSyncStateQuerier{state: syncState},
	)

	require.Len(t, accounts, 1)
	require.Len(t, accounts[0].Drives, 1)
	require.NotNil(t, accounts[0].Drives[0].SyncState)

	ss := accounts[0].Drives[0].SyncState
	assert.Equal(t, "2026-03-02T10:30:00Z", ss.LastSyncTime)
	assert.Equal(t, "1500", ss.LastSyncDuration)
	assert.Equal(t, 45, ss.FileCount)
	assert.Equal(t, 2, ss.Conflicts)
}

func TestBuildStatusAccountsWith_NilSyncState(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"): {SyncDir: "~/OneDrive"},
		},
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockMetaReader{},
		&mockTokenChecker{state: tokenStateValid},
		&mockSyncStateQuerier{state: nil},
	)

	require.Len(t, accounts, 1)
	assert.Nil(t, accounts[0].Drives[0].SyncState)
}

func TestQuerySyncState_NoDB(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	info := querySyncState("/nonexistent/path/state.db", logger)
	assert.Nil(t, info)
}

func TestQuerySyncState_EmptyDB(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	// Create a minimal DB with the required tables.
	createTestStateDB(t, dbPath)

	info := querySyncState(dbPath, logger)
	require.NotNil(t, info)
	assert.Empty(t, info.LastSyncTime)
	assert.Equal(t, 0, info.FileCount)
	assert.Equal(t, 0, info.Conflicts)
}

func TestQuerySyncState_WithMetadata(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
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

	db.Close()

	info := querySyncState(dbPath, logger)
	require.NotNil(t, info)
	assert.Equal(t, "2026-03-02T10:30:00Z", info.LastSyncTime)
	assert.Equal(t, "1500", info.LastSyncDuration)
	assert.Empty(t, info.LastError)
	assert.Equal(t, 1, info.FileCount)
	assert.Equal(t, 1, info.Conflicts)
}

func TestComputeSummary_Mixed(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Drives: []statusDrive{
				{State: driveStateReady, SyncState: &syncStateInfo{Conflicts: 3}},
				{State: driveStatePaused},
			},
		},
		{
			Drives: []statusDrive{
				{State: driveStateNoToken},
				{State: driveStateNeedsSetup},
				{State: driveStateReady, SyncState: &syncStateInfo{Conflicts: 1}},
			},
		},
	}

	s := computeSummary(accounts)
	assert.Equal(t, 5, s.TotalDrives)
	assert.Equal(t, 2, s.Ready)
	assert.Equal(t, 1, s.Paused)
	assert.Equal(t, 1, s.NeedsSetup)
	assert.Equal(t, 1, s.NoToken)
	assert.Equal(t, 4, s.TotalConflicts)
}

func TestComputeSummary_Empty(t *testing.T) {
	t.Parallel()

	s := computeSummary(nil)
	assert.Equal(t, 0, s.TotalDrives)
	assert.Equal(t, 0, s.TotalConflicts)
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
			Email:      "alice@example.com",
			DriveType:  "personal",
			TokenState: tokenStateValid,
			Drives: []statusDrive{
				{
					CanonicalID: "personal:alice@example.com",
					SyncDir:     "~/OneDrive",
					State:       driveStateReady,
					SyncState:   &syncStateInfo{FileCount: 10, Conflicts: 1},
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
	printStatusText(&buf, nil)

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
			TokenState:  tokenStateValid,
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
	printStatusText(&buf, accounts)

	output := buf.String()
	assert.Contains(t, output, "Alice Smith (alice@contoso.com)")
	assert.Contains(t, output, "Org:   Contoso")
	assert.Contains(t, output, "~/Work")
}

func TestPrintStatusText_SyncStateNever(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Email:      "bob@example.com",
			DriveType:  "personal",
			TokenState: tokenStateValid,
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
	printStatusText(&buf, accounts)

	output := buf.String()
	assert.Contains(t, output, "Last sync: never")
}

func TestPrintStatusText_SyncStateWithError(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Email:      "bob@example.com",
			DriveType:  "personal",
			TokenState: tokenStateValid,
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
	printStatusText(&buf, accounts)

	output := buf.String()
	assert.Contains(t, output, "Last error: network timeout")
}

func TestPrintStatusText_EmptySyncDir(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Email:      "bob@example.com",
			DriveType:  "personal",
			TokenState: tokenStateValid,
			Drives: []statusDrive{
				{
					CanonicalID: "personal:bob@example.com",
					SyncDir:     "",
					State:       driveStateNeedsSetup,
				},
			},
		},
	}

	var buf bytes.Buffer
	printStatusText(&buf, accounts)

	output := buf.String()
	assert.Contains(t, output, syncDirNotSet)
}

func TestPrintSummaryText_AllStates(t *testing.T) {
	t.Parallel()

	s := statusSummary{
		TotalDrives:    5,
		Ready:          2,
		Paused:         1,
		NeedsSetup:     1,
		NoToken:        1,
		TotalConflicts: 3,
	}

	var buf bytes.Buffer
	printSummaryText(&buf, s)

	output := buf.String()
	assert.Contains(t, output, "5 drives")
	assert.Contains(t, output, "2 ready")
	assert.Contains(t, output, "1 paused")
	assert.Contains(t, output, "1 needs setup")
	assert.Contains(t, output, "1 no token")
	assert.Contains(t, output, "3 unresolved conflicts")
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
	`)
	require.NoError(t, err)
}
