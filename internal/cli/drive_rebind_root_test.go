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

// Validates: R-6.2.12
func TestRunDriveRebindRoot_RequiresDrive(t *testing.T) {
	setTestDriveHome(t)

	cc := &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      filepath.Join(t.TempDir(), "config.toml"),
	}

	err := runDriveRebindRoot(t.Context(), cc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--drive is required")
}

// Validates: R-6.2.12
func TestRunDriveRebindRoot_KeepsStateAndReportsRebind(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:rebind@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, t.TempDir()))

	statePath := config.DriveStatePath(cid)
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o700))
	store, err := syncengine.NewSyncStore(t.Context(), statePath, testDriveLogger(t))
	require.NoError(t, err)
	require.NoError(t, store.Close(t.Context()))

	var out bytes.Buffer
	cc := &CLIContext{
		Flags:        CLIFlags{Drive: []string{cid.String()}},
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		StatusWriter: &out,
		CfgPath:      cfgPath,
	}

	require.NoError(t, runDriveRebindRoot(t.Context(), cc))
	assert.Contains(t, out.String(), "will use the folder now at its sync path")
	assert.FileExists(t, statePath, "rebinding keeps the sync state")
}

// Validates: R-6.2.12
func TestRunDriveRebindRoot_RefusesLiveSyncOwner(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:rebind@example.com")
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

	err := runDriveRebindRoot(t.Context(), cc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot rebind the sync folder while a sync owner is active")
}
