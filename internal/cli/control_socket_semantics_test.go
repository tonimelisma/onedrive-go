package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"
)

const controlSocketTestReadHeaderTimeout = 5 * time.Second

func startCLIControlSocket(
	t *testing.T,
	status synccontrol.StatusResponse,
	mutate func(http.ResponseWriter, *http.Request),
) {
	t.Helper()

	startCLIControlSocketWithStatusHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		assert.NoError(t, json.NewEncoder(w).Encode(status))
	}, mutate)
}

func startCLIControlSocketWithStatusHandler(
	t *testing.T,
	statusHandler func(http.ResponseWriter, *http.Request),
	mutate func(http.ResponseWriter, *http.Request),
) {
	t.Helper()

	socketPath, err := config.ControlSocketPath()
	require.NoError(t, err)
	require.NotEmpty(t, socketPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(socketPath), 0o700))
	require.NoError(t, os.RemoveAll(socketPath))

	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "unix", socketPath)
	require.NoError(t, err)

	server := &http.Server{
		ReadHeaderTimeout: controlSocketTestReadHeaderTimeout,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == synccontrol.PathStatus {
				statusHandler(w, r)
				return
			}
			mutate(w, r)
		}),
	}

	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("test control server: %v", err)
		}
	}()

	t.Cleanup(func() {
		assert.NoError(t, server.Shutdown(context.Background()))
		assert.NoError(t, os.RemoveAll(socketPath))
	})
}

func TestNotifyDaemon_ReportsControlSocketPathFailureClearly(t *testing.T) {
	longDataHome := filepath.Join(t.TempDir(), strings.Repeat("very-long-control-root-", 8))
	t.Setenv("XDG_DATA_HOME", longDataHome)
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), strings.Repeat("very-long-runtime-root-", 8)))

	var status bytes.Buffer
	cc := &CLIContext{StatusWriter: &status}

	notifyDaemon(cc)
	assert.Contains(t, status.String(), "control socket unavailable")
	assert.Contains(t, status.String(), "changes take effect on next daemon start")
}

func TestNotifyDaemon_ReportsAmbiguousProbeFailureClearly(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	startCLIControlSocketWithStatusHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("{"))
		assert.NoError(t, err)
	}, func(http.ResponseWriter, *http.Request) {
		t.Fatal("daemon notification should stop before mutating when the owner probe is ambiguous")
	})

	var status bytes.Buffer
	cc := &CLIContext{StatusWriter: &status}

	notifyDaemon(cc)
	assert.Contains(t, status.String(), "control socket probe failed")
	assert.Contains(t, status.String(), "changes take effect on next daemon start")
	assert.NotContains(t, status.String(), "no running daemon found")
}

func TestProbeControlOwner_ClassifiesOutcomes(t *testing.T) {
	t.Run("watch owner", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		startCLIControlSocket(t, synccontrol.StatusResponse{OwnerMode: synccontrol.OwnerModeWatch}, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unexpected mutation", http.StatusInternalServerError)
		})

		probe, err := probeControlOwner(t.Context())
		require.NoError(t, err)
		assert.Equal(t, controlOwnerStateWatchOwner, probe.state)
		require.NotNil(t, probe.client)
	})

	t.Run("one-shot owner", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		startCLIControlSocket(t, synccontrol.StatusResponse{OwnerMode: synccontrol.OwnerModeOneShot}, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unexpected mutation", http.StatusInternalServerError)
		})

		probe, err := probeControlOwner(t.Context())
		require.NoError(t, err)
		assert.Equal(t, controlOwnerStateOneShotOwner, probe.state)
		require.NotNil(t, probe.client)
	})

	t.Run("no socket", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())

		probe, err := probeControlOwner(t.Context())
		require.NoError(t, err)
		assert.Equal(t, controlOwnerStateNoSocket, probe.state)
		assert.Nil(t, probe.client)
	})

	t.Run("path unavailable", func(t *testing.T) {
		longDataHome := filepath.Join(t.TempDir(), strings.Repeat("very-long-control-root-", 8))
		t.Setenv("XDG_DATA_HOME", longDataHome)
		t.Setenv("TMPDIR", filepath.Join(t.TempDir(), strings.Repeat("very-long-runtime-root-", 8)))

		probe, err := probeControlOwner(t.Context())
		require.Error(t, err)
		assert.Equal(t, controlOwnerStatePathUnavailable, probe.state)
		assert.Contains(t, err.Error(), "control socket path")
	})

	t.Run("probe failed", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		startCLIControlSocketWithStatusHandler(t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "status unavailable", http.StatusInternalServerError)
		}, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unexpected mutation", http.StatusInternalServerError)
		})

		probe, err := probeControlOwner(t.Context())
		require.Error(t, err)
		assert.Equal(t, controlOwnerStateProbeFailed, probe.state)
		assert.Contains(t, err.Error(), "unexpected control status response")
	})
}
