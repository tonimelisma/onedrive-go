//go:build e2e && e2e_full

package e2e

import (
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Validates: R-2.8.5
func TestE2E_SyncWatch_WebsocketStartupSmoke(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigWithOptions(t, syncDir, "poll_interval = \"5m\"\nwebsocket = true\n")
	env, eventsPath := withSocketIODebugEvents(t, env)

	h, _ := startFastDaemon(t, cfgPath, env,
		"--drive", drive, "sync", "--download-only", "--watch")

	waitForObserverStarted(t, eventsPath, "remote", 3*time.Minute)
	waitForSocketIOConnected(t, eventsPath, 45*time.Second)

	require.NoError(t, h.Process.Signal(syscall.SIGTERM))
	_ = h.Wait()
}
