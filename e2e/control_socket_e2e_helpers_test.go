//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	controlSocketRequestTimeout = 10 * time.Second
	e2eUnixSocketPathSoftLimit  = 100
)

func postControlSocket(t *testing.T, env map[string]string, requestPath string) {
	t.Helper()

	socketPath := e2eControlSocketPath(env)
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

		time.Sleep(200 * time.Millisecond)
	}

	require.NoError(t, lastErr, "post control socket %s", requestPath)
}

func e2eControlSocketPath(env map[string]string) string {
	dataDir := filepath.Join(env["XDG_DATA_HOME"], "onedrive-go")
	candidate := filepath.Join(dataDir, "control.sock")
	if len(candidate) <= e2eUnixSocketPathSoftLimit {
		return candidate
	}

	sum := sha256.Sum256([]byte(dataDir))
	return filepath.Join(os.TempDir(), "odgo-"+hex.EncodeToString(sum[:])[:16], "control.sock")
}
