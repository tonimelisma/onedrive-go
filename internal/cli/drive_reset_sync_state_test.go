package cli

import (
	"bytes"
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

func TestRunDriveResetSyncStateWithInput_RefusesLiveSyncOwner(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:reset@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, t.TempDir()))

	startCLIControlSocket(t, synccontrol.StatusResponse{
		OwnerMode: synccontrol.OwnerModeWatch,
		Drives:    []string{cid.String()},
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

	err := runDriveResetSyncStateWithInput(t.Context(), cc, bytes.NewBufferString(""), true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot reset sync state while a sync owner is active")
}
