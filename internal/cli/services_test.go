package cli

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func newServiceContext(output *bytes.Buffer, cfgPath string) *CLIContext {
	return &CLIContext{
		Logger:       slog.New(slog.DiscardHandler),
		OutputWriter: output,
		CfgPath:      cfgPath,
	}
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
