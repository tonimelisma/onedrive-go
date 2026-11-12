package cli

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"
)

// A purge deletes every file SQLite keeps for a store, not only the
// database: a log or journal left behind is the drive's state still on disk
// after a command that said it purged it.
//
// Validates: R-3.3.8
func TestDeleteStateDB_RemovesTheWholeFamily(t *testing.T) {
	setTestDriveHome(t)

	statePath := config.DriveStatePath(driveid.MustCanonicalID("personal:user@example.com"))
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o700))
	for _, path := range config.StateDBFamily(statePath) {
		require.NoError(t, os.WriteFile(path, []byte("fake"), 0o600))
	}

	removed, err := deleteStateDB(t.Context(), statePath, "", "rerun", testDriveLogger(t))
	require.NoError(t, err)
	assert.True(t, removed)
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		assert.NoFileExists(t, statePath+suffix)
	}
}

// Validates: R-3.3.8
func TestDeleteStateDB_NothingThereIsNothingToDo(t *testing.T) {
	setTestDriveHome(t)

	statePath := config.DriveStatePath(driveid.MustCanonicalID("personal:user@example.com"))
	removed, err := deleteStateDB(t.Context(), statePath, "", "rerun", testDriveLogger(t))
	require.NoError(t, err)
	assert.False(t, removed)
}

// purgeTestStateContent is what a state DB an engine still has open holds in
// these tests: a purge that keeps the store keeps exactly this.
const purgeTestStateContent = "state the engine is still writing"

// seedPurgeDrive configures cid with a sync folder and a state DB, and
// returns the config path and the state DB path.
func seedPurgeDrive(t *testing.T, cid driveid.CanonicalID) (cfgPath, statePath string) {
	t.Helper()

	cfgPath = filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, t.TempDir()))
	writeTestTokenFile(t, config.DefaultDataDir(), "token_"+cid.DriveType()+"_"+cid.Email()+".json")
	statePath = config.DriveStatePath(cid)
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o700))
	require.NoError(t, os.WriteFile(statePath, []byte(purgeTestStateContent), 0o600))

	return cfgPath, statePath
}

// holdDriveState takes the store lock an engine with the state DB at
// statePath open holds, until the returned release (or the test's end).
func holdDriveState(t *testing.T, statePath string) func() error {
	t.Helper()

	release, err := syncengine.LockDriveForOfflineChange(t.Context(), config.DefaultDataDir(), "", statePath,
		testDriveLogger(t))
	require.NoError(t, err)
	// The release may run on a test control server's goroutine.
	var released atomic.Bool
	t.Cleanup(func() {
		if !released.Load() {
			assert.NoError(t, release())
		}
	})

	return func() error {
		released.Store(true)
		return release()
	}
}

// assertStateKept checks that a purge kept the store an engine holds and
// named the command that finishes it.
func assertStateKept(t *testing.T, err error, statePath, rerun string) {
	t.Helper()

	require.ErrorIs(t, err, syncengine.ErrSyncStoreInUse)
	assert.Contains(t, err.Error(), "kept the sync state")
	assert.Contains(t, err.Error(), "run '"+rerun+"' again")
	data, readErr := os.ReadFile(statePath) //nolint:gosec // test-owned temp path
	require.NoError(t, readErr)
	assert.Equal(t, purgeTestStateContent, string(data), "the store an engine has open is left alone")
}

// A purge never deletes a store an engine still has open, whatever that
// engine syncs: the drive leaves the config, the store stays, and the error
// names the command that finishes the purge once that sync has stopped.
//
// Validates: R-3.3.8, R-6.3.9
func TestDriveRemovePurge_KeepsTheStateWhileAnEngineHoldsIt(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:purge-held@example.com")
	cfgPath, statePath := seedPurgeDrive(t, cid)
	release := holdDriveState(t, statePath)

	var out bytes.Buffer
	cc := newCommandContext(&out, cfgPath)
	cc.Flags.Drive = []string{cid.String()}

	err := runDriveRemoveWithContext(t.Context(), cc, true)
	assertStateKept(t, err, statePath, "onedrive-go drive remove --purge --drive "+cid.String())
	assert.NotContains(t, out.String(), "Purged config and state", "a kept store is not reported as purged")
	cfg, loadErr := config.LoadOrDefault(cfgPath, slog.New(slog.DiscardHandler))
	require.NoError(t, loadErr)
	assert.NotContains(t, cfg.Drives, cid, "the drive left the config first")

	require.NoError(t, release())
	require.NoError(t, runDriveRemoveWithContext(t.Context(), cc, true), "the named command finishes the purge")
	assert.NoFileExists(t, statePath)
}

// Validates: R-3.1.4, R-6.3.9
func TestLogoutPurge_KeepsTheStateWhileAnEngineHoldsIt(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:logout-held@example.com")
	cfgPath, statePath := seedPurgeDrive(t, cid)
	release := holdDriveState(t, statePath)

	var out bytes.Buffer
	cc := newCommandContext(&out, cfgPath)
	cc.Flags.Account = cid.Email()

	err := runLogoutWithContext(t.Context(), cc, true)
	assertStateKept(t, err, statePath, "onedrive-go logout --purge --account "+cid.Email())
	assert.NotContains(t, out.String(), "Purged config and state", "a kept store is not reported as purged")
	assert.NoFileExists(t, config.DriveTokenPath(cid), "the rest of the logout went ahead")

	require.NoError(t, release())
	require.NoError(t, runLogoutWithContext(t.Context(), cc, true), "the named command finishes the purge")
	assert.NoFileExists(t, statePath)
}

// watchThatRetiresOnReload is a running watch whose reload retires the
// drive's runner (here: lets go of its store) and checks, at that moment,
// that the drive has already left the config and its store is still there.
func watchThatRetiresOnReload(
	t *testing.T, cid driveid.CanonicalID, cfgPath, statePath string, retire func() error,
) *atomic.Int32 {
	t.Helper()

	var reloads atomic.Int32
	startCLIControlSocket(t, synccontrol.StatusResponse{
		OwnerMode: synccontrol.OwnerModeWatch,
		Mounts:    []string{cid.String()},
	}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != synccontrol.PathReload {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		reloads.Add(1)
		assert.FileExists(t, statePath, "the store is deleted only after the watch was told")
		cfg, err := config.LoadOrDefault(cfgPath, slog.New(slog.DiscardHandler))
		if assert.NoError(t, err) {
			assert.NotContains(t, cfg.Drives, cid, "the watch is told after the drive left the config")
		}
		assert.NoError(t, retire())
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(synccontrol.MutationResponse{Status: synccontrol.StatusReloaded}))
	})

	return &reloads
}

// The watch is told before the store goes: its reload retires the drive's
// runner, whose engine closes the store, and only then is the store deleted.
// Deleting first removed the store under a runner still writing it.
//
// Validates: R-3.3.7, R-3.3.8, R-6.3.9
func TestDriveRemovePurge_NotifiesTheWatchBeforeDeleting(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:purge-watch@example.com")
	cfgPath, statePath := seedPurgeDrive(t, cid)
	reloads := watchThatRetiresOnReload(t, cid, cfgPath, statePath, holdDriveState(t, statePath))

	var out bytes.Buffer
	cc := newCommandContext(&out, cfgPath)
	cc.Flags.Drive = []string{cid.String()}

	require.NoError(t, runDriveRemoveWithContext(t.Context(), cc, true))
	assert.Equal(t, int32(1), reloads.Load())
	assert.NoFileExists(t, statePath)
	assert.Contains(t, out.String(), "The running 'sync --watch' picked up the change.")
	assert.Contains(t, out.String(), "Purged config and state for "+cid.String()+".")
}

// Validates: R-3.1.4, R-6.3.9
func TestLogoutPurge_NotifiesTheWatchBeforeDeleting(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:logout-watch@example.com")
	cfgPath, statePath := seedPurgeDrive(t, cid)
	reloads := watchThatRetiresOnReload(t, cid, cfgPath, statePath, holdDriveState(t, statePath))

	var out bytes.Buffer
	cc := newCommandContext(&out, cfgPath)
	cc.Flags.Account = cid.Email()

	require.NoError(t, runLogoutWithContext(t.Context(), cc, true))
	assert.Equal(t, int32(1), reloads.Load())
	assert.NoFileExists(t, statePath)
	assert.Contains(t, out.String(), "Purged config and state for "+cid.String()+".")
}

// A drive on its default folder has no sync_dir in the config; `drive remove`
// names the folder it synced, not the empty setting.
//
// Validates: R-3.3.7
func TestDriveRemove_NamesTheFolderTheDriveSynced(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:default-folder@example.com")
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("[\""+cid.String()+"\"]\n"), 0o600))

	var out bytes.Buffer
	cc := newCommandContext(&out, cfgPath)
	cc.Flags.Drive = []string{cid.String()}

	require.NoError(t, runDriveRemoveWithContext(t.Context(), cc, false))
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Sync directory untouched: "+home+string(filepath.Separator))
}

// A write-ahead log left without its database is still the account's
// retained state: logout --purge finds the store by it and deletes it, so
// the purge leaves nothing of the account's sync state on disk.
//
// Validates: R-3.1.4
func TestLogoutPurge_RemovesALoneSidecarOfTheAccount(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:lone-log@example.com")
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_lone-log@example.com.json")
	statePath := config.DriveStatePath(cid)
	require.NoError(t, os.WriteFile(statePath+"-wal", []byte("log of a deleted store"), 0o600))

	var out bytes.Buffer
	cc := newCommandContext(&out, filepath.Join(t.TempDir(), "config.toml"))
	cc.Flags.Account = cid.Email()

	require.NoError(t, runLogoutWithContext(t.Context(), cc, true))
	assert.NoFileExists(t, statePath+"-wal")
	assert.Contains(t, out.String(), "Purged orphaned state DB: "+filepath.Base(statePath))
}
