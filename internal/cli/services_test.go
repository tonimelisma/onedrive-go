package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func newServiceContext(output *bytes.Buffer, cfgPath string) *CLIContext {
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
func TestStatusService_Run_NoAccountsWritesGuidance(t *testing.T) {
	setTestDriveHome(t)

	var out bytes.Buffer
	svc := newStatusService(newServiceContext(&out, t.TempDir()+"/missing-config.toml"))

	require.NoError(t, svc.run())
	assert.Contains(t, out.String(), "No accounts configured")
}

// Validates: R-3.3.5
func TestDriveService_RunAdd_NoSelectorWritesGuidance(t *testing.T) {
	setTestDriveHome(t)

	var out bytes.Buffer
	svc := newDriveService(newServiceContext(&out, t.TempDir()+"/config.toml"))

	require.NoError(t, svc.runAdd(t.Context(), nil))
	assert.Contains(t, out.String(), "drive add <canonical-id>")
	assert.Contains(t, out.String(), "drive list")
}

// Validates: R-3.6.2
func TestDriveService_RunSearch_NoBusinessAccounts(t *testing.T) {
	setTestDriveHome(t)

	var out bytes.Buffer
	svc := newDriveService(newServiceContext(&out, t.TempDir()+"/config.toml"))

	err := svc.runSearch(t.Context(), "marketing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no business accounts found")
}

// Validates: R-3.1.4
func TestAuthService_RunLogout_NoAccountsConfigured(t *testing.T) {
	setTestDriveHome(t)

	var out bytes.Buffer
	cc := newServiceContext(&out, t.TempDir()+"/config.toml")
	svc := newAuthService(cc)

	err := svc.runLogout(false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no accounts configured")
}

// Validates: R-2.7
func TestVerifyService_Run_WritesConfiguredOutput(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:test@example.com")
	stateDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", stateDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(config.DriveStatePath(cid)), 0o700))

	var out bytes.Buffer
	cc := &CLIContext{
		Logger:       slog.New(slog.DiscardHandler),
		OutputWriter: &out,
		Cfg: &config.ResolvedDrive{
			CanonicalID: cid,
			SyncDir:     t.TempDir(),
		},
	}

	require.NoError(t, newVerifyService(cc).run(t.Context()))
	assert.Contains(t, out.String(), "All files verified successfully.")
}

// Validates: R-2.7, R-2.7.1
func TestVerifyService_Run_MismatchJSONUsesConfiguredOutput(t *testing.T) {
	setTestDriveHome(t)

	syncDir := t.TempDir()
	_, cid, dbPath := setupVerifyFixture(t, syncDir)
	require.NoError(t, os.WriteFile(filepath.Join(syncDir, "keep.txt"), []byte("keep"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(syncDir, "tamper.txt"), []byte("hello"), 0o600))
	keepHash, err := driveops.ComputeQuickXorHash(filepath.Join(syncDir, "keep.txt"))
	require.NoError(t, err)
	insertVerifyBaselineRows(t, dbPath,
		verifyBaselineRow{
			path:      "keep.txt",
			localHash: keepHash,
			localSize: 4,
		},
		verifyBaselineRow{
			path:      "missing.txt",
			localHash: "expected-missing",
			localSize: 7,
		},
		verifyBaselineRow{
			path:      "tamper.txt",
			localHash: "wrong-hash",
			localSize: 5,
		},
	)

	var out bytes.Buffer
	cc := &CLIContext{
		Flags:        CLIFlags{JSON: true},
		Logger:       slog.New(slog.DiscardHandler),
		OutputWriter: &out,
		Cfg: &config.ResolvedDrive{
			CanonicalID: cid,
			SyncDir:     syncDir,
		},
	}

	err = newVerifyService(cc).run(t.Context())
	require.ErrorIs(t, err, errVerifyMismatch)

	var report synctypes.VerifyReport
	require.NoError(t, json.Unmarshal(out.Bytes(), &report))
	assert.Equal(t, 1, report.Verified)
	require.Len(t, report.Mismatches, 2)
	tamperActualHash, err := driveops.ComputeQuickXorHash(filepath.Join(syncDir, "tamper.txt"))
	require.NoError(t, err)
	assert.Equal(t, []synctypes.VerifyResult{
		{Path: "missing.txt", Status: "missing", Expected: "expected-missing", Actual: ""},
		{Path: "tamper.txt", Status: "hash_mismatch", Expected: "wrong-hash", Actual: tamperActualHash},
	}, report.Mismatches)
}

// Validates: R-2.6
func TestSyncControlService_RunPause_PersistsTimedPause(t *testing.T) {
	setTestDriveHome(t)

	var out bytes.Buffer
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:pause@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive"))

	cc := newServiceContext(&out, cfgPath)
	cc.Flags.Drive = []string{cid.String()}

	svc := newSyncControlService(cc)
	now := time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }

	require.NoError(t, svc.runPause([]string{"2h"}))

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
func TestSyncControlService_RunResume_ClearsPausedKeys(t *testing.T) {
	setTestDriveHome(t)

	var out bytes.Buffer
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:resume@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive"))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "paused", "true"))

	cc := newServiceContext(&out, cfgPath)
	cc.Flags.Drive = []string{cid.String()}

	require.NoError(t, newSyncControlService(cc).runResume())

	cfg, err := config.LoadOrDefault(cfgPath, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	assert.Nil(t, cfg.Drives[cid].Paused)
	assert.Nil(t, cfg.Drives[cid].PausedUntil)
	assert.Contains(t, out.String(), "resumed")
}

// Validates: R-1.9
func TestRecycleBinService_RunList_PersonalAccountMessage(t *testing.T) {
	setTestDriveHome(t)

	var out bytes.Buffer
	svc := newRecycleBinService(newServiceContext(&out, filepath.Join(t.TempDir(), "config.toml")))
	svc.session = func(context.Context) (recycleBinSession, error) {
		return &mockRecycleBinSession{listErr: graph.ErrBadRequest}, nil
	}

	err := svc.runList(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Personal OneDrive accounts")
}

// Validates: R-1.9
func TestRecycleBinService_RunEmpty_FallsBackToDelete(t *testing.T) {
	setTestDriveHome(t)

	var out bytes.Buffer
	mockSession := &mockRecycleBinSession{
		items:              []graph.Item{{ID: "item-1", Name: "old.txt"}},
		permanentDeleteErr: graph.ErrMethodNotAllowed,
	}

	svc := newRecycleBinService(newServiceContext(&out, filepath.Join(t.TempDir(), "config.toml")))
	svc.session = func(context.Context) (recycleBinSession, error) {
		return mockSession, nil
	}

	require.NoError(t, svc.runEmpty(t.Context(), true))
	assert.Equal(t, []string{"item-1"}, mockSession.deletedIDs)
	assert.Contains(t, out.String(), "Recycle bin emptied")
}
