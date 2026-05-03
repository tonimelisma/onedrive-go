package cli

import (
	"bytes"
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

	require.NoError(t, runStatusCommand(cc, false))

	var decoded statusOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	require.Len(t, decoded.Accounts, 1)
	require.Len(t, decoded.Accounts[0].Mounts, 1)
	mount := decoded.Accounts[0].Mounts[0]
	assert.Equal(t, "watch", mount.RuntimeOwner)
	assert.Equal(t, statusRuntimeStateActive, mount.RuntimeState)
	assert.Equal(t, "watch", decoded.Summary.RuntimeOwner)
	assert.Equal(t, 1, decoded.Summary.RuntimeActiveMounts)
}

// Validates: R-2.3.3
func TestStatusRuntimeOverlayApplyAndSummary(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{{
		Mounts: []statusMount{{
			MountID:     "parent",
			CanonicalID: "parent",
			State:       driveStateReady,
			ChildMounts: []statusMount{{MountID: "child"}, {MountID: "idle-child"}},
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
	err := printMountStatus(&out, &statusMount{
		MountID:      "personal:user@example.com",
		CanonicalID:  "personal:user@example.com",
		DisplayName:  "user@example.com",
		SyncDir:      "~/OneDrive",
		State:        driveStateReady,
		RuntimeOwner: "watch",
		RuntimeState: statusRuntimeStateActive,
	}, false)

	require.NoError(t, err)
	assert.Contains(t, out.String(), "Runtime:  watch active")
}
