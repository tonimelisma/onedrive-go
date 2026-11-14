package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"
)

// Validates: R-2.3.3
func TestStatusRuntime_SummaryJSONWithActiveOwner(t *testing.T) {
	setTestDriveHome(t)
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cid := driveid.MustCanonicalID("personal:runtime@example.com")
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive"))
	seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
		account.DisplayName = "Runtime User"
	})
	seedCatalogDrive(t, cid, func(drive *config.CatalogDrive) {
		drive.RemoteDriveID = "drive-runtime"
	})

	startCLIControlSocket(t, synccontrol.StatusResponse{
		OwnerMode: synccontrol.OwnerModeWatch,
		Mounts:    []string{cid.String()},
	}, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unexpected request", http.StatusNotFound)
	})

	var out bytes.Buffer
	cc := newCommandContext(&out, cfgPath)
	cc.Flags.JSON = true

	require.NoError(t, runStatusCommand(t.Context(), cc, false))

	var decoded statusOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	require.Len(t, decoded.Accounts, 1)
	require.Len(t, decoded.Accounts[0].Drives, 1)
	drive := decoded.Accounts[0].Drives[0]
	assert.Equal(t, driveStateNotSynced, drive.State)
	assert.Equal(t, "watch", decoded.Summary.RuntimeOwner)
	assert.Equal(t, 1, decoded.Summary.RuntimeActiveDrives)
}

// busyRuntimeStatusCommand configures one drive whose running sync holds the
// control socket but answers every status request with busy, and returns a
// status command context whose stderr is the second buffer.
func busyRuntimeStatusCommand(t *testing.T) (*CLIContext, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	setTestDriveHome(t)
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cid := driveid.MustCanonicalID("personal:busy-runtime@example.com")
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive"))
	seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
		account.DisplayName = "Busy Runtime User"
	})
	seedCatalogDrive(t, cid, func(drive *config.CatalogDrive) {
		drive.RemoteDriveID = "drive-busy-runtime"
	})

	startCLIControlSocketWithStatusHandler(t, answerControlBusy(t), refuseControlMutation(t))

	var out, status bytes.Buffer
	cc := newCommandContext(&out, cfgPath)
	cc.StatusWriter = &status

	return cc, &out, &status
}

// Validates: R-2.3.3, R-2.9.3
//
// A sync owns the socket but did not answer: status used to drop the live
// state without a word, so a drive the watch was syncing read as pending
// with nothing to say why.
func TestRunStatusCommand_SaysWhenTheRunningSyncDidNotAnswer(t *testing.T) {
	cc, out, status := busyRuntimeStatusCommand(t)
	cc.Flags.JSON = true

	require.NoError(t, runStatusCommand(t.Context(), cc, false))

	var decoded statusOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded), "the note goes to stderr, so stdout stays one JSON document")
	assert.Empty(t, decoded.Summary.RuntimeOwner)
	assert.Equal(t, "Note: the running sync did not answer in time, so its live state is not shown.\n", status.String())
}

// Validates: R-2.9.3, R-6.3.6
//
// A status the user stopped says nothing about the running sync: its control
// socket request ended with the command, which is no news about the sync.
// It used to print "could not read the running sync's live state (... context
// canceled)" after the local state.
func TestRunStatusCommand_AStoppedStatusSaysNothingAboutTheRunningSync(t *testing.T) {
	cc, out, status := busyRuntimeStatusCommand(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.NoError(t, runStatusCommand(ctx, cc, false))
	assert.Contains(t, out.String(), "Account: Busy Runtime User <busy-runtime@example.com>")
	assert.Empty(t, status.String())
}

// Validates: R-2.3.3
func TestStatusRuntimeOverlayApplyAndSummary(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{{
		Mounts: []statusDrive{{
			MountID:     "parent",
			CanonicalID: "parent",
			State:       driveStateReady,
			ChildMounts: []statusDrive{{MountID: "child"}, {MountID: "idle-child"}},
		}},
	}}

	applyStatusRuntimeOverlay(accounts, statusRuntimeOverlay{
		ownerMode: synccontrol.OwnerModeWatch,
		activeMounts: map[string]struct{}{
			"parent": {},
			"child":  {},
		},
	})

	parent := accounts[0].Mounts[0]
	assert.Equal(t, "watch", parent.RuntimeOwner)
	assert.Equal(t, statusRuntimeStateActive, parent.RuntimeState)
	assert.Equal(t, statusRuntimeStateActive, parent.ChildMounts[0].RuntimeState)
	assert.Equal(t, statusRuntimeStateInactive, parent.ChildMounts[1].RuntimeState)

	summary := computeSummary(accounts)
	assert.Equal(t, "watch", summary.RuntimeOwner)
	assert.Equal(t, 2, summary.RuntimeActiveMounts)
}

// Validates: R-2.3.3
func TestPrintMountStatus_ShowsRuntimeState(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	err := printMountStatus(&out, &statusDrive{
		MountID:      "personal:user@example.com",
		CanonicalID:  "personal:user@example.com",
		DisplayName:  "user@example.com",
		SyncDir:      "~/OneDrive",
		State:        driveStateReady,
		RuntimeOwner: "watch",
		RuntimeState: statusRuntimeStateActive,
	})

	require.NoError(t, err)
	assert.NotContains(t, out.String(), "Runtime:")
}
