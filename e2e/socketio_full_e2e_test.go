//go:build e2e && e2e_full

package e2e

import (
	"fmt"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.8.5
func TestE2E_SyncWatch_WebsocketRemoteWakeAndRestart(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigWithOptions(t, syncDir, "poll_interval = \"5m\"\nwebsocket = true\n")
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-watch-websocket-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	h := startDaemonWithStderr(t, cfgPath, env,
		"--drive", drive, "sync", "--download-only", "--watch", "--force")

	waitForSocketIOConnected(t, h.Stderr, 45*time.Second)

	runCLIWithConfig(t, opsCfgPath, nil, "mkdir", "/"+testFolder)

	start := time.Now()
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/first.txt", "first websocket wake")
	firstLocalPath := filepath.Join(syncDir, testFolder, "first.txt")
	pollLocalFileContent(t, firstLocalPath, "first websocket wake", 90*time.Second)
	assert.Less(t, time.Since(start), 2*time.Minute, "remote change should arrive well before the 5-minute fallback poll")

	require.NoError(t, h.Cmd.Process.Signal(syscall.SIGTERM))
	_ = h.Cmd.Wait()

	h2 := startDaemonWithStderr(t, cfgPath, env,
		"--drive", drive, "sync", "--download-only", "--watch", "--force")

	waitForSocketIOConnected(t, h2.Stderr, 45*time.Second)

	restartStart := time.Now()
	putRemoteFile(t, opsCfgPath, nil, "/"+testFolder+"/second.txt", "second websocket wake")
	secondLocalPath := filepath.Join(syncDir, testFolder, "second.txt")
	pollLocalFileContent(t, secondLocalPath, "second websocket wake", 90*time.Second)
	assert.Less(t, time.Since(restartStart), 2*time.Minute, "post-restart remote change should still arrive well before fallback polling")

	require.NoError(t, h2.Cmd.Process.Signal(syscall.SIGTERM))
	_ = h2.Cmd.Wait()
}
