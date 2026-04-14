package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"
)

// Validates: R-2.10.5
func TestControlDaemonErrorFormatting(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "daemon exploded", (&controlDaemonError{message: "daemon exploded"}).Error())
	assert.Equal(t, "internal_error", (&controlDaemonError{code: synccontrol.ErrorInternal}).Error())
	assert.Equal(t, "internal_error: daemon exploded", (&controlDaemonError{
		code:    synccontrol.ErrorInternal,
		message: "daemon exploded",
	}).Error())
}

// Validates: R-2.10.5
func TestControlSocketErrorClassification(t *testing.T) {
	t.Parallel()

	assert.True(t, isControlSocketUnavailable(syscall.ECONNREFUSED))
	assert.True(t, isControlSocketGone(io.EOF))
	assert.True(t, isControlSocketGone(syscall.EPIPE))
	assert.False(t, isControlSocketGone(errors.New("different")))
}

// Validates: R-2.10.5
func TestControlSocketClientReloadAndPostJSON(t *testing.T) {
	t.Run("one-shot owner is a no-op", func(t *testing.T) {
		client := &controlSocketClient{
			status: synccontrol.StatusResponse{OwnerMode: synccontrol.OwnerModeOneShot},
		}
		require.Equal(t, synccontrol.OwnerModeOneShot, client.ownerMode())
		require.NoError(t, client.reload(t.Context()))
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
				Status: synccontrol.StatusOK,
			})) {
				return
			}
		})

		probe, err := probeControlOwner(t.Context())
		require.NoError(t, err)
		require.NotNil(t, probe.client)
		require.NoError(t, probe.client.reload(t.Context()))
		assert.Equal(t, 1, reloadCalls)
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

		err = probe.client.reload(t.Context())
		require.Error(t, err)

		var daemonErr *controlDaemonError
		require.ErrorAs(t, err, &daemonErr)
		assert.Equal(t, http.StatusInternalServerError, daemonErr.statusCode)
		assert.Equal(t, synccontrol.ErrorInternal, daemonErr.code)
		assert.Equal(t, "reload failed", daemonErr.message)
	})
}

// Validates: R-2.10.5
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
