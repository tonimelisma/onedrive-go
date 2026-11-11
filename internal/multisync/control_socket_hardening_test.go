package multisync

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Validates: R-6.3.3
//
// The socket file is 0600, so other users cannot connect. What that does not
// stop is replacement: anyone who can create entries in the directory can
// unlink the socket and bind their own, and the CLI then talks to them. A
// world-writable directory without the sticky bit is exactly that, and
// MkdirAll will not tighten one that already exists.
func TestStartControlSocketServer_RefusesAWritableDirectory(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "preexisting")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	// Loosened deliberately: this is the attacker-created directory the guard
	// exists to refuse, so the test has to produce one.
	require.NoError(t, os.Chmod(dir, 0o777)) //nolint:gosec // the permissive mode is the condition under test

	_, err := startControlSocketServer(
		t.Context(),
		testControlServerConfig(t, filepath.Join(dir, "control.sock"), http.NewServeMux()),
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "writable by other users")
}

// Validates: R-6.3.3
//
// The guard is checked directly here rather than by binding a socket: a
// temporary directory path on macOS already exceeds the Unix socket length
// limit, which is the very constraint the temp-directory fallback exists for.
func TestAssertControlSocketDirSafe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mode      os.FileMode
		wantError bool
	}{
		{name: "OwnerOnly", mode: 0o700},
		{name: "GroupAndWorldReadable", mode: 0o755},
		{name: "GroupWritable", mode: 0o770, wantError: true},
		{name: "WorldWritable", mode: 0o777, wantError: true},
		{name: "WorldWritableSticky", mode: 0o777 | os.ModeSticky},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := filepath.Join(t.TempDir(), "runtime")
			require.NoError(t, os.MkdirAll(dir, 0o700))
			require.NoError(t, os.Chmod(dir, tt.mode))

			err := assertControlSocketDirSafe(dir)
			if !tt.wantError {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), "writable by other users")
		})
	}
}

// Validates: R-6.3.3
//
// The system temp directory is the case the sticky bit exists for: shared and
// world-writable, but others cannot remove entries they do not own. Refusing
// it would refuse the fallback path the product actually uses.
func TestAssertControlSocketDirSafe_AcceptsTheSystemTempDirectory(t *testing.T) {
	t.Parallel()

	require.NoError(t, assertControlSocketDirSafe(os.TempDir()))
}

// Validates: R-6.3.3
//
// A conventional 0755 data directory must keep working; readability by others
// is not what allows a socket to be replaced.
func TestAssertControlSocketDirSafe_AcceptsAConventionalDataDirectory(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "data")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	// 0755 is the conventional data-directory mode this must keep accepting.
	require.NoError(t, os.Chmod(dir, 0o755)) //nolint:gosec // the mode is the condition under test

	require.NoError(t, assertControlSocketDirSafe(dir))
}

// Validates: R-6.3.3
func TestAssertControlSocketDirSafe_MissingDirectoryIsReported(t *testing.T) {
	t.Parallel()

	err := assertControlSocketDirSafe(filepath.Join(t.TempDir(), "absent"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stat control socket directory")
}

// controlTestGuard bounds every wait in these tests, so a deadline the server
// lacks fails the test instead of hanging it.
const controlTestGuard = 5 * time.Second

// shortControlWait stands in for the owner's real waits (seconds), so a test
// reaches a deadline in milliseconds.
const shortControlWait = 20 * time.Millisecond

// testControlServerConfig serves handler at path with the production
// deadlines and no fatal channel.
func testControlServerConfig(t *testing.T, path string, handler http.Handler) *controlServerConfig {
	t.Helper()

	return &controlServerConfig{
		path:     path,
		handler:  handler,
		logger:   synctest.TestLogger(t),
		timeouts: defaultControlTimeouts(),
	}
}

func newControlTestOrchestrator(t *testing.T) *Orchestrator {
	t.Helper()

	return NewOrchestrator(&OrchestratorConfig{
		Logger:            synctest.TestLogger(t),
		ControlSocketPath: shortControlSocketPath(t),
	})
}

// startWatchControlServer serves orch's control socket as a watch owner whose
// loop is the other end of commands.
func startWatchControlServer(t *testing.T, orch *Orchestrator, commands chan<- controlCommand) {
	t.Helper()

	control, err := orch.startControlServer(t.Context(), synccontrol.OwnerModeWatch, commands, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, control.Close(context.Background()))
	})
}

// takeOneCommand plays a loop that takes one command and is still working on
// it when its answer is due: it never answers.
func takeOneCommand(t *testing.T, commands <-chan controlCommand) <-chan controlCommand {
	t.Helper()

	ctx := t.Context()
	taken := make(chan controlCommand, 1)
	go func() {
		select {
		case cmd := <-commands:
			taken <- cmd
		case <-ctx.Done():
		}
	}()

	return taken
}

// sendControlRequest sends one request and returns the owner's HTTP status and
// decoded answer.
func sendControlRequest(t *testing.T, socketPath, method, path string) (int, synccontrol.MutationResponse) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), controlTestGuard)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, synccontrol.HTTPBaseURL+path, http.NoBody)
	require.NoError(t, err)

	// #nosec G704 -- fixed Unix-domain test socket client.
	resp, err := controlTestClient(socketPath).Do(req)
	require.NoError(t, err, "the owner must answer before the test's guard")
	defer resp.Body.Close()

	var decoded synccontrol.MutationResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&decoded))

	return resp.StatusCode, decoded
}

// Validates: R-2.9.7
func TestControlSocketServer_SetsReadIdleAndHeaderLimits(t *testing.T) {
	t.Parallel()

	socketPath := shortControlSocketPath(t)
	control, err := startControlSocketServer(t.Context(), testControlServerConfig(t, socketPath, http.NewServeMux()))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, control.Close(context.Background()))
	})

	assert.Equal(t, controlHeaderTimeout, control.server.ReadHeaderTimeout)
	assert.Equal(t, controlReadTimeout, control.server.ReadTimeout)
	assert.Equal(t, controlIdleTimeout, control.server.IdleTimeout)
	assert.Equal(t, synccontrol.ReplyWriteTimeout, control.server.WriteTimeout)
	assert.Equal(t, maxControlHeaderBytes, control.server.MaxHeaderBytes)

	// The limit is real: a header block several times the limit is refused
	// before any handler runs.
	ctx, cancel := context.WithTimeout(t.Context(), controlTestGuard)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, synccontrol.HTTPBaseURL+synccontrol.PathStatus, http.NoBody)
	require.NoError(t, err)
	req.Header.Set("X-Padding", strings.Repeat("a", 4*maxControlHeaderBytes))

	// #nosec G704 -- fixed Unix-domain test socket client.
	resp, err := controlTestClient(socketPath).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusRequestHeaderFieldsTooLarge, resp.StatusCode)
}

// Validates: R-2.9.7
//
// A client that sends its headers and then only part of the body it promised
// used to hold a handler and a connection for as long as it liked.
func TestControlSocket_SlowBodyIsCutOff(t *testing.T) {
	t.Parallel()

	orch := newControlTestOrchestrator(t)
	orch.controlTimeouts.read = shortControlWait
	control, err := orch.startControlServer(t.Context(), synccontrol.OwnerModeOneShot, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, control.Close(context.Background()))
	})

	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "unix", orch.cfg.ControlSocketPath)
	require.NoError(t, err)
	defer conn.Close()

	_, err = io.WriteString(conn, "POST "+synccontrol.PathPerfCapture+" HTTP/1.1\r\n"+
		"Host: unix\r\nContent-Type: application/json\r\nContent-Length: 64\r\n\r\n"+
		`{"duration_ms":`)
	require.NoError(t, err)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(controlTestGuard)))
	_, err = io.Copy(io.Discard, conn)
	require.NoError(t, err, "the owner must close the connection, not wait for the rest of the body")
}

// Validates: R-2.9.3, R-2.9.7
//
// A loop that does not take a command (it is applying a long reload, or
// stuck) used to leave the handler waiting for as long as the client stayed.
// Now the owner answers busy, and nothing was changed.
func TestHandleControlRequest_LoopNotAcceptingAnswersBusy(t *testing.T) {
	t.Parallel()

	orch := newControlTestOrchestrator(t)
	orch.controlTimeouts.accept = shortControlWait
	// Nobody reads commands: the loop is busy.
	startWatchControlServer(t, orch, make(chan controlCommand))

	requests := []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: synccontrol.PathStatus},
		{method: http.MethodPost, path: synccontrol.PathReload},
		{method: http.MethodPost, path: synccontrol.PathStop},
	}
	for _, request := range requests {
		statusCode, response := sendControlRequest(t, orch.cfg.ControlSocketPath, request.method, request.path)
		assert.Equal(t, http.StatusServiceUnavailable, statusCode, request.path)
		assert.Equal(t, synccontrol.StatusError, response.Status, request.path)
		assert.Equal(t, synccontrol.ErrorBusy, response.Code, request.path)
		assert.NotEmpty(t, response.Message, request.path)
	}
}

// Validates: R-2.9.3, R-2.9.7
//
// A reload the loop took is applied whether or not the client is still
// waiting, so the honest answer past the deadline is "in progress", never a
// silence the client reads as a failure.
func TestHandleControlRequest_SlowReloadAnswersInProgress(t *testing.T) {
	t.Parallel()

	orch := newControlTestOrchestrator(t)
	orch.controlTimeouts.reload = shortControlWait
	commands := make(chan controlCommand)
	taken := takeOneCommand(t, commands)
	startWatchControlServer(t, orch, commands)

	statusCode, response := sendControlRequest(t, orch.cfg.ControlSocketPath, http.MethodPost, synccontrol.PathReload)
	assert.Equal(t, http.StatusAccepted, statusCode)
	assert.Equal(t, synccontrol.StatusReloadInProgress, response.Status)
	assert.Empty(t, response.Code)

	cmd := <-taken
	assert.Equal(t, controlCommandReload, cmd.kind, "the loop took the reload before the owner answered")
}

// Validates: R-2.9.7
//
// The server-wide read and write timeouts bound a request that is answered at
// once; a handler that waits for the loop must still be able to answer long
// after them. The handler moves its own write deadline to cover its wait. The
// read timeout needs no move: net/http clears the read deadline when the
// handler starts, and a handler that set one shorter than its wait would
// have its request context canceled under it.
func TestHandleControlRequest_LongWaitOutlivesTheServerTimeouts(t *testing.T) {
	t.Parallel()

	orch := newControlTestOrchestrator(t)
	orch.controlTimeouts.read = shortControlWait
	orch.controlTimeouts.write = shortControlWait
	orch.controlTimeouts.reload = 10 * shortControlWait
	commands := make(chan controlCommand)
	takeOneCommand(t, commands)
	startWatchControlServer(t, orch, commands)

	statusCode, response := sendControlRequest(t, orch.cfg.ControlSocketPath, http.MethodPost, synccontrol.PathReload)
	assert.Equal(t, http.StatusAccepted, statusCode)
	assert.Equal(t, synccontrol.StatusReloadInProgress, response.Status)
}

// Validates: R-2.9.7
//
// A watch owner whose control server stopped could no longer be paused,
// reloaded or queried, and said so in one warning. The end of Serve now goes
// to the owner's fatal channel.
func TestStartControlSocketServer_UnexpectedServeEndIsFatal(t *testing.T) {
	t.Parallel()

	fatal := make(chan error, 1)
	serverCfg := testControlServerConfig(t, shortControlSocketPath(t), http.NewServeMux())
	serverCfg.fatal = fatal
	control, err := startControlSocketServer(t.Context(), serverCfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, control.Close(context.Background()))
	})

	// Closing the listener under Serve is an end nobody asked for.
	require.NoError(t, control.listener.Close())

	select {
	case serveErr := <-fatal:
		require.ErrorIs(t, serveErr, net.ErrClosed)
		require.ErrorContains(t, serveErr, "control channel stopped")
	case <-time.After(controlTestGuard):
		require.Fail(t, "the end of Serve was not reported as fatal")
	}
}
