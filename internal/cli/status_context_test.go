package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// statusCommandFixture seeds one configured drive and returns the config path.
func statusCommandFixture(t *testing.T, email string) string {
	t.Helper()
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:" + email)
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive"))
	seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
		account.DisplayName = "Context User"
	})
	seedCatalogDrive(t, cid, func(drive *config.CatalogDrive) {
		drive.RemoteDriveID = "drive-context"
	})

	return cfgPath
}

// The status command must run its live overlay under the caller's context so a
// SIGINT cancels the OAuth token refresh and Graph calls it performs, rather
// than under a detached background context that no signal can reach.
//
// Validates: R-6.3.6
func TestStatusCommand_LiveOverlayRunsUnderCommandContext(t *testing.T) {
	cfgPath := statusCommandFixture(t, "overlay-ctx@example.com")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var out bytes.Buffer
	cc := newCommandContext(&out, cfgPath)

	var overlayCtx context.Context
	cc.statusLiveOverlayLoader = func(
		loaderCtx context.Context,
		_ *CLIContext,
		_ accountViewSnapshot,
	) map[string]statusAccountLiveOverlay {
		overlayCtx = loaderCtx
		return nil
	}

	require.NoError(t, runStatusCommand(ctx, cc, false))
	require.NotNil(t, overlayCtx, "live overlay loader must receive a context")
	require.NoError(t, overlayCtx.Err(), "overlay context must still be live before cancellation")

	cancel()
	assert.ErrorIs(t, overlayCtx.Err(), context.Canceled,
		"canceling the command context must cancel the live overlay context")
}

// A command context that is already canceled must stop the status command
// before it reaches the network, instead of completing a full live refresh.
//
// Validates: R-6.3.6
func TestStatusCommand_CanceledContextSkipsLiveOverlayWork(t *testing.T) {
	cfgPath := statusCommandFixture(t, "canceled-ctx@example.com")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var out bytes.Buffer
	cc := newCommandContext(&out, cfgPath)

	overlayCalled := false
	cc.statusLiveOverlayLoader = func(
		loaderCtx context.Context,
		_ *CLIContext,
		_ accountViewSnapshot,
	) map[string]statusAccountLiveOverlay {
		overlayCalled = true
		assert.ErrorIs(t, loaderCtx.Err(), context.Canceled,
			"overlay must observe the canceled command context")
		return nil
	}

	err := runStatusCommand(ctx, cc, false)
	require.NoError(t, err, "status renders from local state even when canceled")
	assert.True(t, overlayCalled, "overlay loader still runs; it observes cancellation itself")
}
