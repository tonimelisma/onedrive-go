//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

const (
	controlSocketRequestTimeout = 10 * time.Second
)

func postControlSocket(t *testing.T, env map[string]string, requestPath string) {
	t.Helper()

	socketPath := e2eControlSocketPath(t, env)
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 2 * time.Second,
	}
	defer client.CloseIdleConnections()

	deadline := time.Now().Add(controlSocketRequestTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		request, err := http.NewRequestWithContext(
			context.Background(),
			http.MethodPost,
			"http://onedrive-go"+requestPath,
			bytes.NewReader(nil),
		)
		require.NoError(t, err)

		response, err := client.Do(request)
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if readErr != nil {
				lastErr = fmt.Errorf("read control response: %w", readErr)
			} else if closeErr != nil {
				lastErr = fmt.Errorf("close control response: %w", closeErr)
			} else if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
				return
			} else {
				lastErr = fmt.Errorf(
					"control socket %s returned %s: %s",
					requestPath,
					response.Status,
					strings.TrimSpace(string(body)),
				)
			}
		} else {
			lastErr = err
		}

		sleepForLiveTestPropagation(200 * time.Millisecond)
	}

	require.NoError(t, lastErr, "post control socket %s", requestPath)
}

// reloadDaemonConfig asks the watch daemon to reload and returns once it
// has. A reload is synchronous: the daemon answers only after it applied the
// config (or with 422 if it kept its old settings, which postControlSocket
// treats as a failure). That answer, not a log line, is how a test knows a
// config change such as `pause` or a new drive is in effect.
func reloadDaemonConfig(t *testing.T, env map[string]string) {
	t.Helper()

	postControlSocket(t, env, synccontrol.PathReload)
}

// waitForControlSocketMount waits until the watch daemon reports mount (a
// canonical drive ID) among the mounts it is running.
func waitForControlSocketMount(t *testing.T, env map[string]string, mount string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var status synccontrol.StatusResponse
	for time.Now().Before(deadline) {
		status = getControlSocketStatus(t, env)
		for _, running := range status.Mounts {
			if strings.EqualFold(running, mount) {
				return
			}
		}

		sleepForLiveTestPropagation(500 * time.Millisecond)
	}

	require.Failf(t, "daemon is not running the mount",
		"after %v waiting for %q; control socket reports mounts %v", timeout, mount, status.Mounts)
}

func getControlSocketStatus(t *testing.T, env map[string]string) synccontrol.StatusResponse {
	t.Helper()

	socketPath := e2eControlSocketPath(t, env)
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 2 * time.Second,
	}
	defer client.CloseIdleConnections()

	deadline := time.Now().Add(controlSocketRequestTimeout)
	var (
		lastErr error
		status  synccontrol.StatusResponse
	)
	for time.Now().Before(deadline) {
		request, err := http.NewRequestWithContext(
			context.Background(),
			http.MethodGet,
			"http://onedrive-go"+synccontrol.PathStatus,
			http.NoBody,
		)
		require.NoError(t, err)

		response, err := client.Do(request)
		if err == nil {
			if response.StatusCode == http.StatusOK {
				decodeErr := json.NewDecoder(response.Body).Decode(&status)
				closeErr := response.Body.Close()
				if decodeErr == nil && closeErr == nil {
					return status
				}
				if decodeErr != nil {
					lastErr = fmt.Errorf("decode control status: %w", decodeErr)
				} else {
					lastErr = fmt.Errorf("close control status response: %w", closeErr)
				}
			} else {
				body, readErr := io.ReadAll(response.Body)
				closeErr := response.Body.Close()
				if readErr != nil {
					lastErr = fmt.Errorf("read control status response: %w", readErr)
				} else if closeErr != nil {
					lastErr = fmt.Errorf("close control status response: %w", closeErr)
				} else {
					lastErr = fmt.Errorf(
						"control socket status returned %s: %s",
						response.Status,
						strings.TrimSpace(string(body)),
					)
				}
			}
		} else {
			lastErr = err
		}

		sleepForLiveTestPropagation(200 * time.Millisecond)
	}

	require.NoError(t, lastErr, "get control socket status")
	return status
}

func e2eControlSocketPath(t *testing.T, env map[string]string) string {
	t.Helper()

	dataDir := filepath.Join(env["XDG_DATA_HOME"], "onedrive-go")
	socketPath, err := config.ControlSocketPathForDataDir(dataDir)
	require.NoError(t, err)
	return socketPath
}
