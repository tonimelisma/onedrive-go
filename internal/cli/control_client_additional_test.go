package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"
)

// Validates: R-2.9.3
func TestControlDaemonErrorFormatting(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "daemon exploded", (&controlDaemonError{message: "daemon exploded"}).Error())
	assert.Equal(t, "internal_error", (&controlDaemonError{code: synccontrol.ErrorInternal}).Error())
	assert.Equal(t, "internal_error: daemon exploded", (&controlDaemonError{
		code:    synccontrol.ErrorInternal,
		message: "daemon exploded",
	}).Error())
}

// Validates: R-2.9.3
func TestControlSocketErrorClassification(t *testing.T) {
	t.Parallel()

	assert.True(t, isControlSocketUnavailable(syscall.ECONNREFUSED))
	assert.True(t, isControlSocketGone(io.EOF))
	assert.True(t, isControlSocketGone(syscall.EPIPE))
	assert.False(t, isControlSocketGone(errors.New("different")))

	// An owner that said busy, or a deadline that passed while it was
	// thinking, is an owner that did not answer; anything else is not.
	assert.True(t, controlOwnerDidNotAnswer(fmt.Errorf("send: %w", &controlDaemonError{code: synccontrol.ErrorBusy})))
	assert.True(t, controlOwnerDidNotAnswer(fmt.Errorf("send: %w", context.DeadlineExceeded)))
	assert.True(t, controlOwnerDidNotAnswer(fmt.Errorf("read: %w", os.ErrDeadlineExceeded)))
	assert.False(t, controlOwnerDidNotAnswer(&controlDaemonError{code: synccontrol.ErrorReloadFailed}))
	assert.False(t, controlOwnerDidNotAnswer(fmt.Errorf("send: %w", context.Canceled)))
	assert.False(t, controlOwnerDidNotAnswer(io.EOF))
}

// Validates: R-2.9.3
func TestControlSocketClientReloadAndPostJSON(t *testing.T) {
	// The client must outlast every wait the owner may spend before it
	// answers, or it gives up first and never hears busy or in-progress.
	t.Run("reload outlasts the owner's waits", func(t *testing.T) {
		ownerWaits := synccontrol.LoopAcceptTimeout + synccontrol.ReloadAnswerTimeout + synccontrol.ReplyWriteTimeout
		assert.Greater(t, controlReloadClientTimeout, ownerWaits)
	})

	t.Run("watch owner success", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		reloadCalls := 0
		startCLIControlSocket(t, synccontrol.StatusResponse{OwnerMode: synccontrol.OwnerModeWatch}, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != synccontrol.PathReload {
				http.Error(w, "unexpected path", http.StatusNotFound)
				return
			}

			reloadCalls++
			w.Header().Set("Content-Type", "application/json")
			if !assert.NoError(t, json.NewEncoder(w).Encode(synccontrol.MutationResponse{
				Status: synccontrol.StatusReloaded,
			})) {
				return
			}
		})

		probe, err := probeControlOwner(t.Context())
		require.NoError(t, err)
		require.NotNil(t, probe.client)
		reloadStatus, err := probe.client.reload(t.Context())
		require.NoError(t, err)
		assert.Equal(t, synccontrol.StatusReloaded, reloadStatus)
		assert.Equal(t, 1, reloadCalls)
	})

	t.Run("watch owner still applying the reload", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		startCLIControlSocket(t, synccontrol.StatusResponse{OwnerMode: synccontrol.OwnerModeWatch}, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			assert.NoError(t, json.NewEncoder(w).Encode(synccontrol.MutationResponse{
				Status: synccontrol.StatusReloadInProgress,
			}))
		})

		probe, err := probeControlOwner(t.Context())
		require.NoError(t, err)
		require.NotNil(t, probe.client)
		reloadStatus, err := probe.client.reload(t.Context())
		require.NoError(t, err, "in progress is an answer, not a failure")
		assert.Equal(t, synccontrol.StatusReloadInProgress, reloadStatus)
	})

	t.Run("watch owner busy", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		startCLIControlSocket(t, synccontrol.StatusResponse{OwnerMode: synccontrol.OwnerModeWatch}, answerControlBusy(t))

		probe, err := probeControlOwner(t.Context())
		require.NoError(t, err)
		require.NotNil(t, probe.client)
		_, err = probe.client.reload(t.Context())

		var daemonErr *controlDaemonError
		require.ErrorAs(t, err, &daemonErr)
		assert.Equal(t, http.StatusServiceUnavailable, daemonErr.statusCode)
		assert.Equal(t, synccontrol.ErrorBusy, daemonErr.code)
		assert.True(t, controlOwnerDidNotAnswer(err))
	})

	t.Run("watch owner daemon error", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		startCLIControlSocket(t, synccontrol.StatusResponse{OwnerMode: synccontrol.OwnerModeWatch}, func(w http.ResponseWriter, r *http.Request) {
			if !assert.Equal(t, synccontrol.PathReload, r.URL.Path) {
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			if !assert.NoError(t, json.NewEncoder(w).Encode(synccontrol.MutationResponse{
				Status:  synccontrol.StatusError,
				Code:    synccontrol.ErrorInternal,
				Message: "reload failed",
			})) {
				return
			}
		})

		probe, err := probeControlOwner(t.Context())
		require.NoError(t, err)
		require.NotNil(t, probe.client)

		_, err = probe.client.reload(t.Context())
		require.Error(t, err)

		var daemonErr *controlDaemonError
		require.ErrorAs(t, err, &daemonErr)
		assert.Equal(t, http.StatusInternalServerError, daemonErr.statusCode)
		assert.Equal(t, synccontrol.ErrorInternal, daemonErr.code)
		assert.Equal(t, "reload failed", daemonErr.message)
	})
}

// Validates: R-2.9.3
func TestDecodeControlJSONResponseHandlesSuccessAndDecodeFailure(t *testing.T) {
	t.Parallel()

	var success synccontrol.MutationResponse
	require.NoError(t, decodeControlJSONResponse(&http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"status":"ok","message":"done"}`)),
	}, &success))
	assert.Equal(t, synccontrol.StatusOK, success.Status)
	assert.Equal(t, "done", success.Message)

	err := decodeControlJSONResponse(&http.Response{
		StatusCode: http.StatusBadRequest,
		Status:     "400 Bad Request",
		Body:       io.NopCloser(strings.NewReader(`{`)),
	}, &synccontrol.MutationResponse{})
	require.Error(t, err)

	var daemonErr *controlDaemonError
	require.ErrorAs(t, err, &daemonErr)
	assert.Equal(t, http.StatusBadRequest, daemonErr.statusCode)
	assert.Equal(t, synccontrol.ErrorInternal, daemonErr.code)
	assert.Contains(t, daemonErr.message, "decode control response")
}
