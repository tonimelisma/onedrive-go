//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fastSyncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (sb *fastSyncBuffer) Write(p []byte) (int, error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	sb.buf = append(sb.buf, p...)
	return len(p), nil
}

func (sb *fastSyncBuffer) String() string {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	return string(sb.buf)
}

func startFastDaemon(t *testing.T, cfgPath string, env map[string]string, args ...string) *exec.Cmd {
	t.Helper()

	daemonArgs := []string{"--config", cfgPath, "--debug"}
	daemonArgs = append(daemonArgs, args...)

	env, eventsPath := ensureDebugEvents(t, env)
	cmd := makeCmd(daemonArgs, env)
	var stdout, stderr fastSyncBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		stopDaemon(t, cmd)

		logCLIExecution(t, daemonArgs, stdout.String(), stderr.String())
	})

	waitForDaemonReady(t, eventsPath)
	return cmd
}

// Validates: R-2.8.5
func TestE2E_SyncWatch_WebsocketDisabledLongPollRegression(t *testing.T) {
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfigWithOptions(t, syncDir, "poll_interval = \"5m\"\nwebsocket = false\n")
	env, eventsPath := withSocketIODebugEvents(t, env)
	opsCfgPath := writeMinimalConfig(t)

	testFolder := fmt.Sprintf("e2e-watch-nowebsocket-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })

	daemon := startFastDaemon(t, cfgPath, env,
		"--drive", drive, "sync", "--watch", "--upload-only")

	const uploadContent = "websocket disabled regression\n"
	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "upload.txt"),
		[]byte(uploadContent),
		0o600,
	))

	// Wait for the full size, not only the name: the upload creates the file
	// empty first, and stopping the daemon in between leaves it empty
	// (LI-20261110-01).
	waitForRemoteStatSize(t, opsCfgPath, nil, "/"+testFolder+"/upload.txt", int64(len(uploadContent)), 3*time.Minute)

	require.NoError(t, daemon.Process.Signal(syscall.SIGTERM))
	waitForDaemonExit(t, daemon)
	assertNoSocketIOConnected(t, eventsPath)
}
