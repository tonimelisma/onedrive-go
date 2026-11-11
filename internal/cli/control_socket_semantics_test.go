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

// controlTestClientDeadline stands in for the client's real timeouts
// (seconds), so a test of an owner that never answers ends in milliseconds.
const controlTestClientDeadline = 100 * time.Millisecond

// controlTestBusyNote is what a config-changing command prints when the
// running watch did not answer.
const controlTestBusyNote = "The running 'sync --watch' is busy; it will pick up the change when it is free.\n"

type controlTestHandler = func(http.ResponseWriter, *http.Request)

// answerWatchStatus is a watch owner that answers status at once.
func answerWatchStatus(t *testing.T) controlTestHandler {
	t.Helper()

	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		assert.NoError(t, json.NewEncoder(w).Encode(synccontrol.StatusResponse{OwnerMode: synccontrol.OwnerModeWatch}))
	}
}

// answerControlBusy is a watch owner whose loop did not take the request.
func answerControlBusy(t *testing.T) controlTestHandler {
	t.Helper()

	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		assert.NoError(t, json.NewEncoder(w).Encode(synccontrol.MutationResponse{
			Status:  synccontrol.StatusError,
			Code:    synccontrol.ErrorBusy,
			Message: "the running sync is busy and did not take the request; nothing was changed",
		}))
	}
}

// neverAnswerControl is an owner that holds the request until the client
// gives up.
func neverAnswerControl(_ http.ResponseWriter, r *http.Request) {
	<-r.Context().Done()
}

func refuseControlMutation(t *testing.T) controlTestHandler {
	t.Helper()

	return func(w http.ResponseWriter, _ *http.Request) {
		assert.Fail(t, "no control mutation was expected")
		http.Error(w, "unexpected mutation", http.StatusInternalServerError)
	}
}

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
			assert.NoError(t, err, "test control server")
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

	notifyDaemon(t.Context(), cc)
	assert.Contains(t, status.String(), "cannot use its control socket path")
	assert.Contains(t, status.String(), "applies when sync next starts")
}

func TestNotifyDaemon_ReportsAmbiguousProbeFailureClearly(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	startCLIControlSocketWithStatusHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("{"))
		assert.NoError(t, err)
	}, func(w http.ResponseWriter, _ *http.Request) {
		assert.Fail(t, "daemon notification should stop before mutating when the owner probe is ambiguous")
		w.WriteHeader(http.StatusInternalServerError)
	})

	var status bytes.Buffer
	cc := &CLIContext{StatusWriter: &status}

	notifyDaemon(t.Context(), cc)
	assert.Contains(t, status.String(), "could not check for a running 'sync --watch'")
	assert.Contains(t, status.String(), "applies when sync next starts")
	assert.NotContains(t, status.String(), "No 'sync --watch' is running")
}

// Validates: R-2.9.3
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

	// An owner that holds the socket but does not answer is a running sync,
	// not "no sync" and not an ambiguous failure.
	t.Run("busy owner", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		startCLIControlSocketWithStatusHandler(t, answerControlBusy(t), refuseControlMutation(t))

		probe, err := probeControlOwner(t.Context())
		require.Error(t, err)
		assert.Equal(t, controlOwnerStateUnresponsive, probe.state)
		assert.Nil(t, probe.client)
		assert.Contains(t, err.Error(), "busy")
	})

	t.Run("owner that does not answer in time", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		startCLIControlSocketWithStatusHandler(t, neverAnswerControl, refuseControlMutation(t))

		ctx, cancel := context.WithTimeout(t.Context(), controlTestClientDeadline)
		defer cancel()
		probe, err := probeControlOwner(ctx)
		require.Error(t, err)
		assert.Equal(t, controlOwnerStateUnresponsive, probe.state)
		assert.Nil(t, probe.client)
	})
}

// Validates: R-2.9.3
//
// The config change is saved, and a running watch rereads its config file on
// its own, so a watch that is busy will apply it: telling the user it applies
// "when sync next starts" was false. Busy is the owner's answer or the
// client's own deadline, for the reload or for the status probe before it.
func TestNotifyDaemon_BusySyncSaysItWillPickTheChangeUp(t *testing.T) {
	tests := []struct {
		name   string
		status func(t *testing.T) controlTestHandler
		reload func(t *testing.T) controlTestHandler
	}{
		{
			name:   "reload answered busy",
			status: answerWatchStatus,
			reload: answerControlBusy,
		},
		{
			name:   "reload not answered in time",
			status: answerWatchStatus,
			reload: func(*testing.T) controlTestHandler { return neverAnswerControl },
		},
		{
			name:   "status answered busy",
			status: answerControlBusy,
			reload: refuseControlMutation,
		},
		{
			name:   "status not answered in time",
			status: func(*testing.T) controlTestHandler { return neverAnswerControl },
			reload: refuseControlMutation,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			startCLIControlSocketWithStatusHandler(t, tt.status(t), tt.reload(t))

			ctx, cancel := context.WithTimeout(t.Context(), controlTestClientDeadline)
			defer cancel()
			var status bytes.Buffer
			notifyDaemon(ctx, &CLIContext{StatusWriter: &status})

			assert.Equal(t, controlTestBusyNote, status.String())
		})
	}
}

// Validates: R-2.9.3
//
// The owner took the reload and was still applying it when its answer was
// due; the reload finishes on its own, so the change is being applied.
func TestNotifyDaemon_ReloadInProgressSaysSo(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	startCLIControlSocket(t, synccontrol.StatusResponse{OwnerMode: synccontrol.OwnerModeWatch},
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			assert.NoError(t, json.NewEncoder(w).Encode(synccontrol.MutationResponse{
				Status: synccontrol.StatusReloadInProgress,
			}))
		})

	var status bytes.Buffer
	notifyDaemon(t.Context(), &CLIContext{StatusWriter: &status})

	assert.Equal(t, "The running 'sync --watch' is applying the change.\n", status.String())
}

// Validates: R-2.9.3
func TestNotifyDaemon_ReloadRejectedByRunningSyncIsAWarning(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	startCLIControlSocket(t, synccontrol.StatusResponse{OwnerMode: synccontrol.OwnerModeWatch},
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, err := w.Write([]byte(`{"status":"error","code":"reload_failed",` +
				`"message":"the running sync couldn't apply the new config and kept its previous settings: bad value"}`))
			assert.NoError(t, err)
		})

	var status bytes.Buffer
	cc := &CLIContext{StatusWriter: &status}

	notifyDaemon(t.Context(), cc)
	assert.Equal(t,
		"Warning: the running sync couldn't apply the new config and kept its previous settings: bad value\n",
		status.String(), "a rejected reload must not read as success or show the raw error code")
}
