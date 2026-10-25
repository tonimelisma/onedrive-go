//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func containsAny(output string, targets ...string) bool {
	for _, target := range targets {
		if strings.Contains(output, target) {
			return true
		}
	}

	return false
}

func startFastDaemon(t *testing.T, cfgPath string, env map[string]string, args ...string) (*exec.Cmd, *fastSyncBuffer) {
	t.Helper()

	daemonArgs := []string{"--config", cfgPath, "--debug"}
	daemonArgs = append(daemonArgs, args...)

	env, eventsPath := ensureDebugEvents(t, env)
	cmd := makeCmd(daemonArgs, env)
	var stdout fastSyncBuffer
	stderr := &fastSyncBuffer{}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr

	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}

		logCLIExecution(t, daemonArgs, stdout.String(), stderr.String())
	})

	waitForDaemonReady(t, eventsPath, 30*time.Second)
	return cmd, stderr
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

	daemon, _ := startFastDaemon(t, cfgPath, env,
		"--drive", drive, "sync", "--watch", "--upload-only")

	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, "upload.txt"),
		[]byte("websocket disabled regression\n"),
		0o644,
	))

	waitForRemoteReadContains(t, opsCfgPath, nil, "", "upload.txt", 3*time.Minute, "stat", "/"+testFolder+"/upload.txt")

	require.NoError(t, daemon.Process.Signal(syscall.SIGTERM))
	_ = daemon.Wait()
	assertNoSocketIOConnected(t, eventsPath)
}
