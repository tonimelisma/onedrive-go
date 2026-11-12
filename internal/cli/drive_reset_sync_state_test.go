package cli

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"
)

func TestNewDriveResetSyncStateCmd_HasYesFlag(t *testing.T) {
	cmd := newDriveResetSyncStateCmd()

	yesFlag := cmd.Flags().Lookup("yes")
	require.NotNil(t, yesFlag)
	assert.Equal(t, "false", yesFlag.DefValue)
}

func TestRunDriveResetSyncStateWithInput_RequiresDrive(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cc := &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      cfgPath,
	}

	err := runDriveResetSyncStateWithInput(t.Context(), cc, bytes.NewBufferString(""), true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--drive is required")
}

func TestRunDriveResetSyncStateWithInput_RequiresInteractiveConfirmationWithoutYes(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:reset@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, t.TempDir()))

	cc := &CLIContext{
		Flags:        CLIFlags{Drive: []string{cid.String()}},
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      cfgPath,
	}

	err := runDriveResetSyncStateWithInput(t.Context(), cc, bytes.NewBufferString("RESET\n"), false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires confirmation")
}

func TestRunDriveResetSyncStateWithInput_ResetsAndRecreatesStateDB(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:reset@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, t.TempDir()))

	statePath := config.DriveStatePath(cid)
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o700))
	require.NoError(t, os.WriteFile(statePath, []byte("not a sqlite database"), 0o600))

	var out bytes.Buffer
	cc := &CLIContext{
		Flags:        CLIFlags{Drive: []string{cid.String()}},
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		StatusWriter: &out,
		CfgPath:      cfgPath,
	}

	require.NoError(t, runDriveResetSyncStateWithInput(t.Context(), cc, bytes.NewBufferString(""), true))
	assert.Contains(t, out.String(), "Reset sync state DB for "+cid.String()+".")

	store, err := syncengine.NewSyncStore(t.Context(), statePath, testDriveLogger(t))
	require.NoError(t, err)
	require.NoError(t, store.Close(t.Context()))
}

// holdSyncFolder takes, for the rest of the test, the sync-folder lock an
// engine syncing syncDir holds.
func holdSyncFolder(t *testing.T, syncDir string) {
	t.Helper()

	release, err := syncengine.LockDriveForOfflineChange(t.Context(), config.DefaultDataDir(), syncDir,
		filepath.Join(t.TempDir(), "engine.db"), testDriveLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, release()) })
}

// An engine still draining or closing its store after its owner stopped
// answering on the control socket holds only its sync-folder lock. Every
// offline change to a drive's state store waits for that lock too, or it
// rewrites a store the engine is still writing.
//
// Validates: R-6.3.9, R-2.9.5
func TestOfflineStateChanges_RefuseWhileAnEngineHoldsTheFolder(t *testing.T) {
	tests := []struct {
		name string
		run  func(ctx context.Context, cc *CLIContext) error
	}{
		{
			name: "drive reset-sync-state",
			run: func(ctx context.Context, cc *CLIContext) error {
				return runDriveResetSyncStateWithInput(ctx, cc, bytes.NewBufferString(""), true)
			},
		},
		{
			name: "drive rebind-root",
			run: func(ctx context.Context, cc *CLIContext) error {
				return runDriveRebindRootWithInput(ctx, cc, bytes.NewBufferString(""), true)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setTestDriveHome(t)

			cfgPath := filepath.Join(t.TempDir(), "config.toml")
			cid := driveid.MustCanonicalID("personal:offline-change@example.com")
			syncDir := t.TempDir()
			require.NoError(t, config.AppendDriveSection(cfgPath, cid, syncDir))

			statePath := config.DriveStatePath(cid)
			require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o700))
			const kept = "state the engine is still writing"
			require.NoError(t, os.WriteFile(statePath, []byte(kept), 0o600))
			holdSyncFolder(t, syncDir)

			cc := &CLIContext{
				Flags:        CLIFlags{Drive: []string{cid.String()}},
				Logger:       testDriveLogger(t),
				OutputWriter: &bytes.Buffer{},
				StatusWriter: &bytes.Buffer{},
				CfgPath:      cfgPath,
			}

			err := tt.run(t.Context(), cc)
			require.ErrorIs(t, err, syncengine.ErrSyncRootInUse)
			data, readErr := os.ReadFile(statePath) //nolint:gosec // test-owned temp path
			require.NoError(t, readErr)
			assert.Equal(t, kept, string(data), "the state DB is left alone")
		})
	}
}

func TestRunDriveResetSyncStateWithInput_RefusesLiveSyncOwner(t *testing.T) {
	cc := liveSyncOwnerContext(t, "personal:reset@example.com")

	err := runDriveResetSyncStateWithInput(t.Context(), cc, bytes.NewBufferString(""), true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot reset sync state while a sync owner is active")
}

// Validates: R-2.6.5
func TestRunDriveResetSyncStateWithInput_RejectsInvalidCanonicalID(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cc := &CLIContext{
		Flags:        CLIFlags{Drive: []string{"not-a-canonical-id"}},
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      cfgPath,
	}

	err := runDriveResetSyncStateWithInput(t.Context(), cc, bytes.NewBufferString(""), true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no drive matching "not-a-canonical-id"`)
}

func TestRunDriveResetSyncStateWithInput_RejectsUnknownConfiguredDrive(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:reset@example.com")

	cc := &CLIContext{
		Flags:        CLIFlags{Drive: []string{cid.String()}},
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      cfgPath,
	}

	err := runDriveResetSyncStateWithInput(t.Context(), cc, bytes.NewBufferString(""), true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in config")
}

func TestEnsureNoLiveStateResetOwner_AllowsUnmanagedActiveOwner(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	startCLIControlSocket(t, synccontrol.StatusResponse{
		OwnerMode: synccontrol.OwnerModeWatch,
		Mounts:    []string{"personal:someone-else@example.com"},
	}, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unexpected mutation", http.StatusInternalServerError)
	})

	err := ensureNoLiveSyncOwner(t.Context(), driveid.MustCanonicalID("personal:reset@example.com"), "reset sync state")
	require.NoError(t, err)
}

func TestEnsureNoLiveStateResetOwner_ProbeFailureReturnsError(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	startCLIControlSocketWithStatusHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "status unavailable", http.StatusInternalServerError)
	}, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unexpected mutation", http.StatusInternalServerError)
	})

	err := ensureNoLiveSyncOwner(t.Context(), driveid.MustCanonicalID("personal:reset@example.com"), "reset sync state")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "probe control owner")
}

func TestConfirmDriveStateResetIntent_YesSkipsPrompt(t *testing.T) {
	var output bytes.Buffer

	err := confirmDriveStateResetIntent(bytes.NewBufferString("wrong\n"), &output, driveid.MustCanonicalID("personal:reset@example.com"), true)
	require.NoError(t, err)
	assert.Empty(t, output.String())
}

func TestStdinAsWriter_NonWriterReturnsNil(t *testing.T) {
	assert.Nil(t, stdinAsWriter(bytes.NewReader(nil)))
	var buf bytes.Buffer
	assert.Same(t, &buf, stdinAsWriter(&buf))
}

func TestNewDriveResetSyncStateCmd_RunE_UsesYesFlag(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:reset@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, t.TempDir()))

	var output bytes.Buffer
	cc := &CLIContext{
		Flags:        CLIFlags{Drive: []string{cid.String()}},
		Logger:       testDriveLogger(t),
		OutputWriter: &output,
		StatusWriter: &output,
		CfgPath:      cfgPath,
	}

	cmd := newDriveResetSyncStateCmd()
	cmd.SetContext(context.WithValue(t.Context(), cliContextKey{}, cc))
	cmd.SetIn(bytes.NewBufferString(""))
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	require.NoError(t, cmd.Flags().Set("yes", "true"))

	err := cmd.RunE(cmd, nil)
	require.NoError(t, err)
	assert.Contains(t, output.String(), "Reset sync state DB for "+cid.String()+".")
}

// liveSyncOwnerContext configures one drive and a running watch owner that
// holds it, for commands that must refuse to change its state.
func liveSyncOwnerContext(t *testing.T, canonicalID string) *CLIContext {
	t.Helper()
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID(canonicalID)
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, t.TempDir()))

	startCLIControlSocket(t, synccontrol.StatusResponse{
		OwnerMode: synccontrol.OwnerModeWatch,
		Mounts:    []string{cid.String()},
	}, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unexpected mutation", http.StatusInternalServerError)
	})

	cc := &CLIContext{
		Flags:        CLIFlags{Drive: []string{cid.String()}},
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      cfgPath,
	}

	return cc
}
