package multisync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

const (
	controlSocketDirPerm = 0o700

	// controlSocketDirOtherWrite is group or other write permission, which is
	// what lets another user replace entries in the directory.
	controlSocketDirOtherWrite = 0o022
	controlSocketFilePerm      = 0o600
	controlDialTimeout         = 200 * time.Millisecond
	controlCloseTimeout        = time.Second
	controlHeaderTimeout       = 5 * time.Second
	// controlReadTimeout bounds reading one whole request, body included.
	controlReadTimeout = 10 * time.Second
	// controlIdleTimeout closes a keep-alive connection that sends nothing.
	controlIdleTimeout = 30 * time.Second
	// maxControlHeaderBytes bounds a request's header block; the control
	// protocol's headers are a few hundred bytes.
	maxControlHeaderBytes = 16 * 1024
	// maxPerfCaptureDuration bounds a perf capture, which holds a handler, a
	// connection and the profilers for its whole duration.
	maxPerfCaptureDuration = 5 * time.Minute

	// maxControlRequestBytes bounds a control request body.
	maxControlRequestBytes = 64 * 1024
)

// controlTimeouts are the control server's deadlines. Production uses
// defaultControlTimeouts; tests shorten them so a deadline is reached in
// milliseconds instead of seconds.
type controlTimeouts struct {
	header time.Duration // reading a request's headers
	read   time.Duration // reading a whole request, body included
	idle   time.Duration // a keep-alive connection with no request
	accept time.Duration // the watch loop taking a command
	reload time.Duration // a reload the loop took finishing
	write  time.Duration // writing one answer
}

func defaultControlTimeouts() controlTimeouts {
	return controlTimeouts{
		header: controlHeaderTimeout,
		read:   controlReadTimeout,
		idle:   controlIdleTimeout,
		accept: synccontrol.LoopAcceptTimeout,
		reload: synccontrol.ReloadAnswerTimeout,
		write:  synccontrol.ReplyWriteTimeout,
	}
}

// controlServerConfig is what startControlSocketServer serves.
type controlServerConfig struct {
	path     string
	handler  http.Handler
	logger   *slog.Logger
	timeouts controlTimeouts
	// fatal receives an end of Serve nobody asked for. Nil means the owner
	// carries on without its control channel and the end is only logged.
	fatal chan<- error
	// listen binds the socket; nil means listenControlSocket.
	listen func(ctx context.Context, path string) (net.Listener, error)
}

type controlCommandKind int

const (
	controlCommandStatus controlCommandKind = iota
	controlCommandReload
	controlCommandStop
)

type controlCommand struct {
	kind     controlCommandKind
	response chan controlResponse
}

type controlResponse struct {
	StatusCode int
	Code       synccontrol.ErrorCode
	Body       any
	Err        error
}

type ControlSocketInUseError struct {
	Path string
}

// Error names the condition and the way out. The socket path is an internal
// detail kept on the struct for diagnostics; it is not part of the message.
func (e *ControlSocketInUseError) Error() string {
	return "another onedrive-go sync is already running; stop it or wait for it to finish, then try again"
}

type controlSocketServer struct {
	path     string
	server   *http.Server
	listener net.Listener
	done     chan struct{}
	logger   *slog.Logger
	// lock is held for the server's lifetime; it, not the socket file, is
	// what makes this process the owner.
	lock *ownerLock
}

func startControlSocketServer(ctx context.Context, cfg *controlServerConfig) (*controlSocketServer, error) {
	path, handler, logger := cfg.path, cfg.handler, cfg.logger
	if path == "" {
		return nil, fmt.Errorf("control socket path is empty")
	}
	socketDir := filepath.Dir(path)
	if err := localpath.MkdirAll(socketDir, controlSocketDirPerm); err != nil {
		return nil, fmt.Errorf("create control socket directory: %w", err)
	}
	if err := assertControlSocketDirSafe(socketDir); err != nil {
		return nil, err
	}

	lock, err := acquireOwnerLock(path+controlLockSuffix, path)
	if err != nil {
		return nil, err
	}

	listen := cfg.listen
	if listen == nil {
		listen = listenControlSocket
	}
	listener, err := listen(ctx, path)
	if err != nil {
		return nil, errors.Join(err, lock.release())
	}
	if chmodErr := localpath.Chmod(path, controlSocketFilePerm); chmodErr != nil {
		cleanupErr := closeListenerAndRemoveSocket(listener, path)
		return nil, errors.Join(
			fmt.Errorf("set control socket permissions: %w", chmodErr),
			cleanupErr,
			lock.release(),
		)
	}

	server := newControlHTTPServer(handler, cfg.timeouts)
	control := &controlSocketServer{
		path:     path,
		server:   server,
		listener: listener,
		done:     make(chan struct{}),
		logger:   logger,
		lock:     lock,
	}

	// The owner closes the server (Close), and only once its drives have
	// stopped: closing releases the owner lock, so closing it when the
	// context is canceled let a reset or a second sync start while a drive
	// was still draining into its store.
	go func() {
		defer close(control.done)
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			reportControlServeEnd(logger, path, cfg.fatal, serveErr)
		}
	}()

	return control, nil
}

// newControlHTTPServer bounds every connection: reading the headers, reading
// the whole request, an idle keep-alive connection, and the header size. The
// write timeout is the default for an answer that is ready at once; a handler
// that waits (for the watch loop, for a capture) moves its own write deadline
// through extendControlWriteDeadline, because a server-wide budget long
// enough for a five-minute capture would bound nothing else.
func newControlHTTPServer(handler http.Handler, timeouts controlTimeouts) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: timeouts.header,
		ReadTimeout:       timeouts.read,
		WriteTimeout:      timeouts.write,
		IdleTimeout:       timeouts.idle,
		MaxHeaderBytes:    maxControlHeaderBytes,
	}
}

// reportControlServeEnd handles a Serve that returned although nobody closed
// the server. A watch owner without its control channel cannot be paused,
// reloaded or queried, so the end goes to the watch loop's fatal channel and
// the watch stops with it instead of syncing on out of reach. A one-shot
// owner (no fatal channel) keeps its owner lock, finishes its pass and only
// logs.
func reportControlServeEnd(logger *slog.Logger, path string, fatal chan<- error, serveErr error) {
	err := fmt.Errorf("the control channel stopped, so status, pause and resume could no longer reach this sync: %w", serveErr)
	if fatal == nil {
		logger.Warn("The control channel stopped; status, pause and resume can't reach this sync until it restarts",
			slog.String("path", path),
			slog.Any("error", serveErr),
		)

		return
	}

	select {
	case fatal <- err:
	default:
		// Another fatal error is already pending and the watch is ending for
		// it; this one is still worth a line in the log.
		logger.Error("The control channel stopped while the sync was already stopping",
			slog.String("path", path),
			slog.Any("error", serveErr),
		)
	}
}

// extendControlWriteDeadline gives a handler that waits longer than the
// server's write timeout its own budget, measured from now. Only the write
// deadline needs moving: the read timeout bounds reading the request, and
// net/http clears the read deadline when the handler starts (its background
// read for a closed connection runs without one), so a long wait is never
// cut short by it. Setting a read deadline here would bring that cut back.
//
// A failure is logged and otherwise ignored on purpose: the server-wide write
// timeout stays in force and still bounds the connection, so the handler
// risks being cut short, never hanging. On a Unix socket connection it does
// not fail.
func extendControlWriteDeadline(w http.ResponseWriter, budget time.Duration, logger *slog.Logger) {
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(budget)); err != nil {
		logger.Debug("control request keeps the server's write deadline", slog.Any("error", err))
	}
}

// assertControlSocketDirSafe refuses to serve from a directory another user
// could hijack the socket in.
//
// The socket file itself is 0600, so other users cannot connect to it. The
// attack that permission does not stop is replacement: anyone who can create
// entries in the directory can unlink our socket and bind their own, and the
// CLI then talks to them. So the property that matters is who may write to the
// directory, not who may read it.
//
// That distinction matters because MkdirAll only applies its mode to
// directories it creates. The socket path falls back to a name under the
// system temp directory when the data-directory path would exceed the Unix
// socket length limit, and that name is a predictable hash, so a local user
// can create it first and own it.
//
// Safe means the directory is ours or the system's, and is either closed to
// other writers or sticky. The sticky bit is what makes a shared temp
// directory safe: others may create their own entries there but cannot remove
// ours. An attacker-owned directory is refused even when sticky, because its
// owner can always remove what it contains.
func assertControlSocketDirSafe(dir string) error {
	info, err := localpath.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat control socket directory: %w", err)
	}

	mode := info.Mode()
	if mode.Perm()&controlSocketDirOtherWrite != 0 && mode&os.ModeSticky == 0 {
		return fmt.Errorf(
			"control socket directory %s is writable by other users without the sticky bit (mode %#o); "+
				"another user could replace the socket. Run 'chmod 700 %s'",
			dir, mode.Perm(), dir)
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}

	if owner := stat.Uid; owner != uint32(os.Getuid()) && owner != 0 {
		return fmt.Errorf(
			"control socket directory %s is owned by uid %d, not this user or root; "+
				"its owner could replace the socket. Point TMPDIR or XDG_DATA_HOME at a directory only you own",
			dir, owner)
	}

	return nil
}

// listenControlSocket binds the socket. The caller holds the owner lock, so
// a socket file already at path cannot belong to a live owner of this
// version: it is a leftover from a sync that exited without cleaning up, and
// removing it is safe. The dial check stays as a guard against an owner that
// predates the lock.
func listenControlSocket(ctx context.Context, path string) (net.Listener, error) {
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "unix", path)
	if err == nil {
		return listener, nil
	}

	if controlSocketLive(ctx, path) {
		return nil, &ControlSocketInUseError{Path: path}
	}
	if removeErr := localpath.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale control socket %s: %w", path, removeErr)
	}

	listener, err = listenConfig.Listen(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen control socket %s: %w", path, err)
	}

	return listener, nil
}

func controlSocketLive(ctx context.Context, path string) bool {
	dialer := net.Dialer{Timeout: controlDialTimeout}
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return false
	}
	return conn.Close() == nil
}

func (s *controlSocketServer) Close(ctx context.Context) error {
	if s == nil || s.server == nil {
		return nil
	}

	err := s.server.Shutdown(ctx)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	if s.done != nil {
		<-s.done
	}

	if removeErr := localpath.Remove(s.path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		if err == nil {
			err = fmt.Errorf("remove control socket: %w", removeErr)
		} else {
			err = errors.Join(err, fmt.Errorf("remove control socket: %w", removeErr))
		}
	}
	// Release ownership only after the socket is gone, so the next owner
	// never meets this process's socket while holding the lock.
	if lockErr := s.lock.release(); lockErr != nil {
		err = errors.Join(err, lockErr)
	}

	if dirErr := removeEmptyRuntimeSocketDir(s.path); dirErr != nil && s.logger != nil {
		s.logger.Debug("remove empty control socket runtime directory",
			slog.String("path", filepath.Dir(s.path)),
			slog.Any("error", dirErr),
		)
	}

	return err
}

func removeEmptyRuntimeSocketDir(socketPath string) error {
	dir := filepath.Dir(socketPath)
	if filepath.Dir(dir) != filepath.Clean(os.TempDir()) || !strings.HasPrefix(filepath.Base(dir), "odgo-") {
		return nil
	}

	if err := localpath.Remove(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
			return nil
		}
		return fmt.Errorf("remove control socket runtime directory: %w", err)
	}

	return nil
}

func (o *Orchestrator) startControlServer(
	ctx context.Context,
	mode synccontrol.OwnerMode,
	commands chan<- controlCommand,
	fatal chan<- error,
) (*controlSocketServer, error) {
	if o.cfg.ControlSocketPath == "" {
		return &controlSocketServer{}, nil
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.handleControlRequest(w, r, mode, commands)
	})

	return startControlSocketServer(ctx, &controlServerConfig{
		path:     o.cfg.ControlSocketPath,
		handler:  handler,
		logger:   o.logger,
		timeouts: o.controlTimeouts,
		fatal:    fatal,
		listen:   o.controlListen,
	})
}

func (o *Orchestrator) handleControlRequest(
	w http.ResponseWriter,
	r *http.Request,
	mode synccontrol.OwnerMode,
	commands chan<- controlCommand,
) {
	if o.handleDirectPerfControlRequest(w, r, mode) {
		return
	}

	if mode == synccontrol.OwnerModeOneShot {
		o.handleOneShotControlRequest(w, r)
		return
	}

	cmd, ok := parseControlCommand(r)
	if !ok {
		writeJSON(w, http.StatusNotFound, synccontrol.MutationResponse{
			Status:  synccontrol.StatusError,
			Code:    synccontrol.ErrorInvalidRequest,
			Message: "unknown control endpoint or malformed control request",
		})
		return
	}

	timeouts := o.controlTimeouts
	answerWait := controlAnswerWait(cmd.kind, timeouts)
	extendControlWriteDeadline(w, timeouts.accept+answerWait+timeouts.write, o.logger)

	// Two bounded waits with different answers: a command the loop never took
	// changed nothing (busy); a command it took is carried out whether or not
	// anyone is still waiting, so the answer past that deadline must not read
	// as a failure. The response channel is buffered, so a loop answering
	// after the handler left never blocks.
	cmd.response = make(chan controlResponse, 1)
	acceptTimer := time.NewTimer(timeouts.accept)
	defer acceptTimer.Stop()
	select {
	case commands <- cmd:
	case <-acceptTimer.C:
		writeControlResponse(w, controlBusyResponse())
		return
	case <-r.Context().Done():
		return
	}

	answerTimer := time.NewTimer(answerWait)
	defer answerTimer.Stop()
	select {
	case response := <-cmd.response:
		writeControlResponse(w, response)
	case <-answerTimer.C:
		writeControlResponse(w, controlUnansweredResponse(cmd.kind))
	case <-r.Context().Done():
		return
	}
}

// controlAnswerWait is how long a handler waits for the loop's answer to a
// command the loop took. Status and stop are answered the moment the loop
// takes them, so their wait only guards against a loop stuck mid-command.
func controlAnswerWait(kind controlCommandKind, timeouts controlTimeouts) time.Duration {
	if kind == controlCommandReload {
		return timeouts.reload
	}

	return timeouts.accept
}

func controlBusyResponse() controlResponse {
	return controlResponse{
		StatusCode: http.StatusServiceUnavailable,
		Code:       synccontrol.ErrorBusy,
		Err:        fmt.Errorf("the running sync is busy and did not take the request; nothing was changed"),
	}
}

// controlUnansweredResponse answers a command the loop took but had not
// finished when its answer was due. A reload carries on and applies the
// change, so it is "in progress", not an error.
func controlUnansweredResponse(kind controlCommandKind) controlResponse {
	if kind == controlCommandReload {
		return controlResponse{
			StatusCode: http.StatusAccepted,
			Body:       synccontrol.MutationResponse{Status: synccontrol.StatusReloadInProgress},
		}
	}

	return controlBusyResponse()
}

func (o *Orchestrator) handleOneShotControlRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == synccontrol.PathStatus {
		writeJSON(w, http.StatusOK, o.controlStatus(synccontrol.OwnerModeOneShot))
		return
	}

	writeJSON(w, http.StatusConflict, synccontrol.MutationResponse{
		Status:  synccontrol.StatusError,
		Code:    synccontrol.ErrorForegroundSyncRunning,
		Message: "a foreground sync is currently running",
	})
}

func (o *Orchestrator) handleDirectPerfControlRequest(
	w http.ResponseWriter,
	r *http.Request,
	mode synccontrol.OwnerMode,
) bool {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == synccontrol.PathPerfStatus:
		writeJSON(w, http.StatusOK, o.controlPerfStatus(mode))
		return true
	case r.Method == http.MethodPost && r.URL.Path == synccontrol.PathPerfCapture:
		o.handlePerfCaptureRequest(w, r, mode)
		return true
	default:
		return false
	}
}

func (o *Orchestrator) handlePerfCaptureRequest(
	w http.ResponseWriter,
	r *http.Request,
	mode synccontrol.OwnerMode,
) {
	var request synccontrol.PerfCaptureRequest
	// Bounded because the daemon is long-lived and this body is decoded whole.
	// A PerfCaptureRequest is a handful of fields, so the ceiling is far above
	// anything legitimate.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxControlRequestBytes)).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, synccontrol.MutationResponse{
			Status:  synccontrol.StatusError,
			Code:    synccontrol.ErrorInvalidRequest,
			Message: "decode perf capture request: " + err.Error(),
		})
		return
	}

	opts, err := perfCaptureOptionsFromRequest(mode, &request)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, synccontrol.MutationResponse{
			Status:  synccontrol.StatusError,
			Code:    synccontrol.ErrorInvalidRequest,
			Message: err.Error(),
		})
		return
	}

	// The answer's budget is the capture's duration, known only once the
	// body is read; until then the server-wide timeouts hold, so a client
	// that never finishes its body is cut off by the read timeout.
	extendControlWriteDeadline(w, opts.Duration+o.controlTimeouts.write, o.logger)
	result, err := o.capturePerf(r.Context(), opts)
	if err != nil {
		statusCode, code := perfCaptureErrorStatus(err)
		writeJSON(w, statusCode, synccontrol.MutationResponse{
			Status:  synccontrol.StatusError,
			Code:    code,
			Message: err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, synccontrol.PerfCaptureResponse{
		OwnerMode: mode,
		Result:    result,
	})
}

func parseControlCommand(r *http.Request) (controlCommand, bool) {
	return parseRootControlCommand(r)
}

func parseRootControlCommand(r *http.Request) (controlCommand, bool) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == synccontrol.PathStatus:
		return controlCommand{kind: controlCommandStatus}, true
	case r.Method == http.MethodPost && r.URL.Path == synccontrol.PathReload:
		return controlCommand{kind: controlCommandReload}, true
	case r.Method == http.MethodPost && r.URL.Path == synccontrol.PathStop:
		return controlCommand{kind: controlCommandStop}, true
	default:
		return controlCommand{}, false
	}
}

func writeControlResponse(w http.ResponseWriter, response controlResponse) {
	if response.Err != nil {
		status := response.StatusCode
		if status == 0 {
			status = http.StatusInternalServerError
		}
		code := response.Code
		if code == "" {
			code = synccontrol.ErrorInternal
		}
		writeJSON(w, status, synccontrol.MutationResponse{
			Status:  synccontrol.StatusError,
			Code:    code,
			Message: response.Err.Error(),
		})
		return
	}

	writeJSON(w, responseStatus(response.StatusCode), response.Body)
}

func responseStatus(status int) int {
	if status == 0 {
		return http.StatusOK
	}
	return status
}

// writeJSON writes one control answer. An encode error is dropped on
// purpose: the status line has already gone out, so the only failure left is
// a client that went away, and there is nobody left to tell.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		return
	}
}

// controlStatus is the owner's status answer. Before any runner is up (a
// one-shot owner that has not set its mounts yet), it names the configured
// drives; a selection that does not compile names none.
func (o *Orchestrator) controlStatus(mode synccontrol.OwnerMode) synccontrol.StatusResponse {
	mounts := o.controlMountIDs()
	if len(mounts) == 0 {
		configured, err := buildStandaloneMountSpecs(o.cfg.StandaloneMounts)
		if err == nil {
			mounts = mountIDsForSpecs(configured)
		}
	}

	return synccontrol.StatusResponse{
		OwnerMode:               mode,
		Mounts:                  mounts,
		ShortcutCleanupFailures: o.shortcutCleanupDiagnosticSnapshot(),
	}
}

func (o *Orchestrator) controlPerfStatus(mode synccontrol.OwnerMode) synccontrol.PerfStatusResponse {
	status := synccontrol.PerfStatusResponse{
		OwnerMode: mode,
	}
	if o == nil || o.perfRuntime == nil {
		return status
	}

	status.Aggregate = o.perfRuntime.AggregateSnapshot()
	status.Mounts = o.perfRuntime.SnapshotByMount()

	return status
}

func (o *Orchestrator) capturePerf(ctx context.Context, opts perf.CaptureOptions) (perf.CaptureResult, error) {
	if o == nil || o.perfRuntime == nil {
		return perf.CaptureResult{}, perf.ErrCaptureUnavailable
	}

	//nolint:wrapcheck // perf errors already say "capture ..."; a "capture perf:" prefix only stuttered.
	return o.perfRuntime.Capture(ctx, opts)
}

func perfCaptureOptionsFromRequest(
	mode synccontrol.OwnerMode,
	request *synccontrol.PerfCaptureRequest,
) (perf.CaptureOptions, error) {
	if request == nil {
		return perf.CaptureOptions{}, fmt.Errorf("missing perf capture request")
	}

	if request.DurationMS <= 0 {
		return perf.CaptureOptions{}, fmt.Errorf("perf capture duration must be greater than zero")
	}
	// Compared in milliseconds, before the conversion, so a huge request
	// cannot overflow into a small duration that passes.
	if request.DurationMS > maxPerfCaptureDuration.Milliseconds() {
		return perf.CaptureOptions{}, fmt.Errorf("perf capture duration must be at most %s", maxPerfCaptureDuration)
	}
	duration := time.Duration(request.DurationMS) * time.Millisecond
	// The owner runs in its own working directory, so a relative path would
	// not mean what the requester typed. Clients send absolute paths (see
	// synccontrol.PerfCaptureRequest.MarshalJSON).
	if request.OutputDir != "" && !filepath.IsAbs(request.OutputDir) {
		return perf.CaptureOptions{}, fmt.Errorf("perf capture output directory %q must be an absolute path", request.OutputDir)
	}

	return perf.CaptureOptions{
		Duration:   duration,
		OutputDir:  request.OutputDir,
		Trace:      request.Trace,
		FullDetail: request.FullDetail,
		OwnerMode:  string(mode),
	}, nil
}

func perfCaptureErrorStatus(err error) (int, synccontrol.ErrorCode) {
	switch {
	case errors.Is(err, perf.ErrCaptureInProgress):
		return http.StatusConflict, synccontrol.ErrorCaptureInProgress
	case errors.Is(err, perf.ErrCaptureUnavailable):
		return http.StatusConflict, synccontrol.ErrorCaptureUnavailable
	default:
		return http.StatusInternalServerError, synccontrol.ErrorInternal
	}
}

// handleControlCommand answers one command on the watch loop. stopRequested
// is true for a stop; err is set when a reload had to stop a drive that did
// not stop within its bound (ErrSyncStuck), which ends the watch.
func (o *Orchestrator) handleControlCommand(
	ctx context.Context,
	cmd *controlCommand,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	runners map[mountID]*watchRunner,
	runnerEvents chan<- watchRunnerEvent,
	childWork *parentChildWorkSnapshots,
) (stopRequested bool, err error) {
	switch cmd.kind {
	case controlCommandStatus:
		cmd.response <- controlResponse{Body: o.controlStatus(synccontrol.OwnerModeWatch)}
	case controlCommandReload:
		reloadErr := o.reload(ctx, mode, opts, runners, runnerEvents, childWork)
		switch {
		case errors.Is(reloadErr, ErrSyncStuck):
			// The config may be partly applied and the sync is ending; the
			// client must not read either as a settled reload.
			cmd.response <- controlResponse{
				StatusCode: http.StatusInternalServerError,
				Code:       synccontrol.ErrorInternal,
				Err:        fmt.Errorf("the running sync is stopping: %w", reloadErr),
			}
			return false, reloadErr
		case reloadErr != nil:
			// The client that asked for the reload (pause, resume, drive add,
			// login) must learn the running sync kept its old settings;
			// answering "reloaded" here made those commands report success.
			cmd.response <- controlResponse{
				StatusCode: http.StatusUnprocessableEntity,
				Code:       synccontrol.ErrorReloadFailed,
				Err:        fmt.Errorf("the running sync couldn't apply the new config and kept its previous settings: %w", reloadErr),
			}
			return false, nil
		}
		cmd.response <- controlResponse{Body: synccontrol.MutationResponse{Status: synccontrol.StatusReloaded}}
	case controlCommandStop:
		cmd.response <- controlResponse{Body: synccontrol.MutationResponse{Status: synccontrol.StatusStopping}}
		return true, nil
	}

	return false, nil
}

func closeListenerAndRemoveSocket(listener net.Listener, path string) error {
	var err error
	if closeErr := listener.Close(); closeErr != nil {
		err = fmt.Errorf("close control socket listener: %w", closeErr)
	}
	if removeErr := localpath.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		removeErr = fmt.Errorf("remove control socket: %w", removeErr)
		if err == nil {
			err = removeErr
		} else {
			err = errors.Join(err, removeErr)
		}
	}

	return err
}
