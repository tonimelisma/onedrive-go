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
	controlSocketDirPerm  = 0o700
	controlSocketFilePerm = 0o600
	controlDialTimeout    = 200 * time.Millisecond
	controlCloseTimeout   = time.Second
	controlHeaderTimeout  = 5 * time.Second
)

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

func (e *ControlSocketInUseError) Error() string {
	return fmt.Sprintf("another sync process is already running (control socket %s is live)", e.Path)
}

type controlSocketServer struct {
	path     string
	server   *http.Server
	listener net.Listener
	done     chan struct{}
	logger   *slog.Logger
}

func startControlSocketServer(
	ctx context.Context,
	path string,
	handler http.Handler,
	logger *slog.Logger,
) (*controlSocketServer, error) {
	if path == "" {
		return nil, fmt.Errorf("control socket path is empty")
	}
	if err := localpath.MkdirAll(filepath.Dir(path), controlSocketDirPerm); err != nil {
		return nil, fmt.Errorf("create control socket directory: %w", err)
	}

	listener, err := listenControlSocket(ctx, path)
	if err != nil {
		return nil, err
	}
	if chmodErr := localpath.Chmod(path, controlSocketFilePerm); chmodErr != nil {
		cleanupErr := closeListenerAndRemoveSocket(listener, path)
		return nil, errors.Join(
			fmt.Errorf("set control socket permissions: %w", chmodErr),
			cleanupErr,
		)
	}

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: controlHeaderTimeout,
	}
	control := &controlSocketServer{
		path:     path,
		server:   server,
		listener: listener,
		done:     make(chan struct{}),
		logger:   logger,
	}

	go func() {
		defer close(control.done)
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			logger.Error("control socket server exited",
				slog.String("path", path),
				slog.String("error", serveErr.Error()),
			)
		}
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), controlCloseTimeout)
		defer cancel()
		if closeErr := control.Close(shutdownCtx); closeErr != nil {
			logger.Warn("close control socket after context cancellation",
				slog.String("path", path),
				slog.String("error", closeErr.Error()),
			)
		}
	}()

	return control, nil
}

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

	if dirErr := removeEmptyRuntimeSocketDir(s.path); dirErr != nil && s.logger != nil {
		s.logger.Debug("remove empty control socket runtime directory",
			slog.String("path", filepath.Dir(s.path)),
			slog.String("error", dirErr.Error()),
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
) (*controlSocketServer, error) {
	if o.cfg.ControlSocketPath == "" {
		return &controlSocketServer{}, nil
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.handleControlRequest(w, r, mode, commands)
	})

	return startControlSocketServer(ctx, o.cfg.ControlSocketPath, handler, o.logger)
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

	cmd.response = make(chan controlResponse, 1)
	select {
	case commands <- cmd:
	case <-r.Context().Done():
		return
	}

	select {
	case response := <-cmd.response:
		writeControlResponse(w, response)
	case <-r.Context().Done():
		return
	}
}

func (o *Orchestrator) handleOneShotControlRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == synccontrol.PathStatus {
		writeJSON(w, http.StatusOK, o.controlStatus(r.Context(), synccontrol.OwnerModeOneShot))
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
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
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

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		return
	}
}

func (o *Orchestrator) controlStatus(ctx context.Context, mode synccontrol.OwnerMode) synccontrol.StatusResponse {
	_ = ctx
	mounts := o.controlMountIDs()
	if len(mounts) == 0 {
		configured, err := buildConfiguredMountSpecs(resolvedDrivesWithSelection(o.cfg.Drives))
		if err == nil {
			mounts = mountIDsForSpecs(configured)
		}
	}

	return synccontrol.StatusResponse{
		OwnerMode: mode,
		Mounts:    mounts,
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

	result, err := o.perfRuntime.Capture(ctx, opts)
	if err != nil {
		return perf.CaptureResult{}, fmt.Errorf("capture perf: %w", err)
	}

	return result, nil
}

func perfCaptureOptionsFromRequest(
	mode synccontrol.OwnerMode,
	request *synccontrol.PerfCaptureRequest,
) (perf.CaptureOptions, error) {
	if request == nil {
		return perf.CaptureOptions{}, fmt.Errorf("missing perf capture request")
	}

	duration := time.Duration(request.DurationMS) * time.Millisecond
	if duration <= 0 {
		return perf.CaptureOptions{}, fmt.Errorf("perf capture duration must be greater than zero")
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

func (o *Orchestrator) handleControlCommand(
	ctx context.Context,
	cmd *controlCommand,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	runners map[mountID]*watchRunner,
) bool {
	switch cmd.kind {
	case controlCommandStatus:
		cmd.response <- controlResponse{Body: o.controlStatus(ctx, synccontrol.OwnerModeWatch)}
	case controlCommandReload:
		o.reload(ctx, mode, opts, runners)
		cmd.response <- controlResponse{Body: synccontrol.MutationResponse{Status: synccontrol.StatusReloaded}}
	case controlCommandStop:
		cmd.response <- controlResponse{Body: synccontrol.MutationResponse{Status: synccontrol.StatusStopping}}
		return true
	}

	return false
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
