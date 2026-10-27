package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
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

	err := runDriveRebindRootWithInput(t.Context(), cc, bytes.NewBufferString(""), true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--drive is required")
}

// Validates: R-6.2.12
func TestRunDriveRebindRoot_KeepsStateAndReportsRebind(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:rebind@example.com")
	syncDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(syncDir, "a.txt"), []byte("a"), 0o600))
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, syncDir))

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

	require.NoError(t, runDriveRebindRootWithInput(t.Context(), cc, bytes.NewBufferString(""), true))
	assert.Contains(t, out.String(), "remembers 0 files")
	assert.Contains(t, out.String(), "holds 1 files now")
	assert.Contains(t, out.String(), "will use the folder now at its sync path")
	assert.FileExists(t, statePath, "rebinding keeps the sync state")
}

// Validates: R-6.2.12
func TestRunDriveRebindRoot_RefusesLiveSyncOwner(t *testing.T) {
	cc := liveSyncOwnerContext(t, "personal:rebind@example.com")

	err := runDriveRebindRootWithInput(t.Context(), cc, bytes.NewBufferString(""), true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot rebind the sync folder while a sync owner is active")
}

// Validates: R-6.2.12
func TestRunDriveRebindRoot_RequiresConfirmationWithoutYes(t *testing.T) {
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

	err = runDriveRebindRootWithInput(t.Context(), cc, bytes.NewBufferString("REBIND\n"), false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires confirmation")
	assert.Contains(t, out.String(), "remembers 0 files", "the counts are shown before asking")
}

// Validates: R-6.2.12
func TestRunDriveRebindRoot_UnreadableStateStillRebindsWithYes(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:rebind@example.com")
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

	err := runDriveRebindRootWithInput(t.Context(), cc, bytes.NewBufferString(""), true)
	assert.Contains(t, out.String(), "remembers an unknown number of files")
	if err != nil {
		assert.NotContains(t, err.Error(), "read sync state", "an unreadable count must not block the rebind")
	}
}
