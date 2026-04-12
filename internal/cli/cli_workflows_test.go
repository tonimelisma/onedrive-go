package cli

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func newCommandContext(output *bytes.Buffer, cfgPath string) *CLIContext {
	return &CLIContext{
		Logger:       slog.New(slog.DiscardHandler),
		OutputWriter: output,
		StatusWriter: output,
		CfgPath:      cfgPath,
	}
}

type mockRecycleBinSession struct {
	items              []graph.Item
	listErr            error
	restoreItem        *graph.Item
	restoreErr         error
	permanentDeleteErr error
	deleteErr          error
	deletedIDs         []string
}

func (m *mockRecycleBinSession) ListRecycleBinItems(context.Context) ([]graph.Item, error) {
	return m.items, m.listErr
}

func (m *mockRecycleBinSession) RestoreItem(context.Context, string) (*graph.Item, error) {
	return m.restoreItem, m.restoreErr
}

func (m *mockRecycleBinSession) PermanentDeleteItem(_ context.Context, itemID string) error {
	if m.permanentDeleteErr == nil {
		m.deletedIDs = append(m.deletedIDs, itemID)
	}

	return m.permanentDeleteErr
}

func (m *mockRecycleBinSession) DeleteItem(_ context.Context, itemID string) error {
	if m.deleteErr == nil {
		m.deletedIDs = append(m.deletedIDs, itemID)
	}

	return m.deleteErr
}

// Validates: R-4.8.4
func TestStatusCommand_NoAccountsWritesGuidance(t *testing.T) {
	setTestDriveHome(t)

	var out bytes.Buffer
	cc := newCommandContext(&out, t.TempDir()+"/missing-config.toml")

	require.NoError(t, runStatusCommand(cc, false))
	assert.Contains(t, out.String(), "No accounts configured")
}

// Validates: R-3.3.5
func TestDriveAdd_NoSelectorWritesGuidance(t *testing.T) {
	setTestDriveHome(t)

	var out bytes.Buffer
	cc := newCommandContext(&out, t.TempDir()+"/config.toml")

	require.NoError(t, runDriveAddWithContext(t.Context(), cc, nil))
	assert.Contains(t, out.String(), "drive add <canonical-id>")
	assert.Contains(t, out.String(), "drive list")
}

// Validates: R-3.6.2
func TestDriveSearch_NoBusinessAccounts(t *testing.T) {
	setTestDriveHome(t)

	var out bytes.Buffer
	cc := newCommandContext(&out, t.TempDir()+"/config.toml")

	err := runDriveSearchWithContext(t.Context(), cc, "marketing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no business accounts found")
}

// Validates: R-3.3.9, R-3.7
func TestDriveSearch_RefreshesIdentityOnceBeforeSharePointSearch(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("business:alice@contoso.com")
	require.NoError(t, config.SaveAccountProfile(cid, &config.AccountProfile{
		UserID:      "user-123",
		DisplayName: "Alice Smith",
	}))
	writeAccessTokenFile(t, cid, "token-business-search")

	var meCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case testGraphMePath:
			meCalls.Add(1)
			writeTestResponse(t, w, `{
				"id":"user-123",
				"displayName":"Alice Smith",
				"mail":"alice@contoso.com"
			}`)
		case "/sites":
			assert.Contains(t, r.URL.RawQuery, "search=marketing")
			writeTestResponse(t, w, `{"value":[]}`)
		default:
			assert.Failf(t, "unexpected graph path", "path=%s", r.URL.Path)
			http.Error(w, "unexpected graph path", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	var out bytes.Buffer
	cc := newCommandContext(&out, filepath.Join(t.TempDir(), "missing-config.toml"))
	cc.GraphBaseURL = srv.URL

	require.NoError(t, runDriveSearchWithContext(t.Context(), cc, "marketing"))
	assert.Equal(t, int32(1), meCalls.Load())
	assert.Contains(t, out.String(), `No SharePoint sites found matching "marketing".`)
}

// Validates: R-3.1.4
func TestLogoutCommand_NoAccountsConfigured(t *testing.T) {
	setTestDriveHome(t)

	var out bytes.Buffer
	cc := newCommandContext(&out, t.TempDir()+"/config.toml")
	err := runLogoutWithContext(cc, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no accounts configured")
}

// Validates: R-3.1.4
func TestLogoutCommand_PurgeRemovesAccountProfile(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("business:alice@contoso.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive - Contoso"))
	require.NoError(t, config.SaveAccountProfile(cid, &config.AccountProfile{
		DisplayName: "Alice Smith",
		OrgName:     "Contoso",
	}))
	writeTestTokenFile(t, config.DefaultDataDir(), "token_business_alice@contoso.com.json")
	require.NoError(t, os.WriteFile(config.DriveStatePath(cid), []byte("fake-db"), 0o600))
	require.NoError(t, os.WriteFile(config.DriveMetadataPath(cid), []byte(`{"drive_id":"d1"}`), 0o600))

	syncDir := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, os.MkdirAll(syncDir, 0o700))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "sync_dir", syncDir))

	var out bytes.Buffer
	cc := newCommandContext(&out, cfgPath)
	cc.Flags.Account = "alice@contoso.com"

	require.NoError(t, runLogoutWithContext(cc, true))

	_, tokenErr := os.Stat(config.DriveTokenPath(cid))
	assert.True(t, os.IsNotExist(tokenErr), "logout --purge should remove token file")

	_, stateErr := os.Stat(config.DriveStatePath(cid))
	assert.True(t, os.IsNotExist(stateErr), "logout --purge should remove state DB")

	_, metaErr := os.Stat(config.DriveMetadataPath(cid))
	assert.True(t, os.IsNotExist(metaErr), "logout --purge should remove drive metadata")

	_, profileErr := os.Stat(config.AccountFilePath(cid))
	assert.True(t, os.IsNotExist(profileErr), "logout --purge should remove account profile")

	_, syncDirErr := os.Stat(syncDir)
	require.NoError(t, syncDirErr, "logout --purge must leave sync directories untouched")
	assert.Contains(t, out.String(), "Sync directories untouched")
}

// Validates: R-3.3.8, R-3.1.5
func TestDriveRemove_PurgePreservesAccountProfile(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("business:alice@contoso.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive - Contoso"))
	require.NoError(t, config.SaveAccountProfile(cid, &config.AccountProfile{
		DisplayName: "Alice Smith",
		OrgName:     "Contoso",
	}))
	writeTestTokenFile(t, config.DefaultDataDir(), "token_business_alice@contoso.com.json")
	require.NoError(t, os.WriteFile(config.DriveStatePath(cid), []byte("fake-db"), 0o600))
	require.NoError(t, os.WriteFile(config.DriveMetadataPath(cid), []byte(`{"drive_id":"d1"}`), 0o600))

	var out bytes.Buffer
	cc := newCommandContext(&out, cfgPath)
	cc.Flags.Drive = []string{cid.String()}

	require.NoError(t, runDriveRemoveWithContext(cc, true))

	cfg, err := config.LoadOrDefault(cfgPath, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	assert.Empty(t, cfg.Drives, "drive remove --purge should delete only the drive config section")

	_, stateErr := os.Stat(config.DriveStatePath(cid))
	assert.True(t, os.IsNotExist(stateErr), "drive remove --purge should remove the drive state DB")

	_, metaErr := os.Stat(config.DriveMetadataPath(cid))
	assert.True(t, os.IsNotExist(metaErr), "drive remove --purge should remove drive metadata")

	_, tokenErr := os.Stat(config.DriveTokenPath(cid))
	require.NoError(t, tokenErr, "drive remove --purge must preserve the account token")

	profile, found, profileErr := config.LookupAccountProfile(cid)
	require.NoError(t, profileErr)
	require.True(t, found, "drive remove --purge must preserve the account profile")
	assert.Equal(t, "Alice Smith", profile.DisplayName)

	catalog := buildAccountCatalog(t.Context(), config.DefaultConfig(), testDriveLogger(t))
	entry := accountCatalogEntryByEmail(t, catalog, "alice@contoso.com")
	assert.False(t, entry.Configured)
	assert.Equal(t, "Alice Smith", entry.DisplayName)
	assert.Equal(t, authstate.StateReady, entry.AuthHealth.State)
	assert.Contains(t, out.String(), "Sync directory untouched")
}

// Validates: R-2.6
func TestPauseCommand_PersistsTimedPause(t *testing.T) {
	setTestDriveHome(t)

	var out bytes.Buffer
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:pause@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive"))

	cc := newCommandContext(&out, cfgPath)
	cc.Flags.Drive = []string{cid.String()}

	now := time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC)

	require.NoError(t, runPauseCommand(cc, func() time.Time { return now }, []string{"2h"}))

	cfg, err := config.LoadOrDefault(cfgPath, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	drive := cfg.Drives[cid]
	require.NotNil(t, drive.Paused)
	assert.True(t, *drive.Paused)
	require.NotNil(t, drive.PausedUntil)
	assert.Equal(t, now.Add(2*time.Hour).Format(time.RFC3339), *drive.PausedUntil)
	assert.Contains(t, out.String(), "paused until")
}

// Validates: R-2.6
func TestResumeCommand_ClearsPausedKeys(t *testing.T) {
	setTestDriveHome(t)

	var out bytes.Buffer
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:resume@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive"))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused", "true"))

	cc := newCommandContext(&out, cfgPath)
	cc.Flags.Drive = []string{cid.String()}

	require.NoError(t, runResumeCommand(cc, time.Now))

	cfg, err := config.LoadOrDefault(cfgPath, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	assert.Nil(t, cfg.Drives[cid].Paused)
	assert.Nil(t, cfg.Drives[cid].PausedUntil)
	assert.Contains(t, out.String(), "resumed")
}

// Validates: R-1.9
func TestRecycleBinList_PersonalAccountMessage(t *testing.T) {
	setTestDriveHome(t)

	var out bytes.Buffer
	cc := newCommandContext(&out, filepath.Join(t.TempDir(), "config.toml"))
	sessionFactory := func(context.Context) (recycleBinSession, error) {
		return &mockRecycleBinSession{listErr: graph.ErrBadRequest}, nil
	}

	err := runRecycleBinListWithFactory(t.Context(), cc, sessionFactory)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Personal OneDrive accounts")
}

// Validates: R-1.9
func TestRecycleBinEmpty_FallsBackToDelete(t *testing.T) {
	setTestDriveHome(t)

	var out bytes.Buffer
	mockSession := &mockRecycleBinSession{
		items:              []graph.Item{{ID: "item-1", Name: "old.txt"}},
		permanentDeleteErr: graph.ErrMethodNotAllowed,
	}

	cc := newCommandContext(&out, filepath.Join(t.TempDir(), "config.toml"))
	sessionFactory := func(context.Context) (recycleBinSession, error) {
		return mockSession, nil
	}

	require.NoError(t, runRecycleBinEmptyWithFactory(t.Context(), cc, true, sessionFactory))
	assert.Equal(t, []string{"item-1"}, mockSession.deletedIDs)
	assert.Contains(t, out.String(), "Recycle bin emptied")
}
