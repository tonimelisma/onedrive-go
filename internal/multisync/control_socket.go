package multisync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

const (
	controlSocketDirPerm  = 0o700
	controlSocketFilePerm = 0o600
	controlDialTimeout    = 200 * time.Millisecond
	controlCloseTimeout   = time.Second
	controlHeaderTimeout  = 5 * time.Second
	controlStatusError    = "error"
	controlStatusPath     = "/v1/status"
)

type controlOwnerMode string

const (
	controlOwnerOneShot controlOwnerMode = "oneshot"
	controlOwnerWatch   controlOwnerMode = "watch"
)

type controlCommandKind int

const (
	controlCommandStatus controlCommandKind = iota
	controlCommandReload
	controlCommandStop
	controlCommandApproveHeldDeletes
	controlCommandRequestConflictResolution
)

type controlCommand struct {
	kind       controlCommandKind
	driveID    driveid.CanonicalID
	conflictID string
	resolution string
	response   chan controlResponse
}

type controlResponse struct {
	StatusCode int
	Body       any
	Err        error
}

type controlStatusResponse struct {
	OwnerMode string   `json:"owner_mode"`
	Drives    []string `json:"drives"`
}

type controlMutationResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type controlConflictRequestBody struct {
	Resolution string `json:"resolution"`
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

	return err
}

func (o *Orchestrator) startControlServer(
	ctx context.Context,
	mode controlOwnerMode,
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
	mode controlOwnerMode,
	commands chan<- controlCommand,
) {
	if mode == controlOwnerOneShot {
		o.handleOneShotControlRequest(w, r)
		return
	}

	cmd, ok := parseControlCommand(r)
	if !ok {
		http.NotFound(w, r)
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
	if r.Method == http.MethodGet && r.URL.Path == controlStatusPath {
		writeJSON(w, http.StatusOK, controlStatusResponse{
			OwnerMode: string(controlOwnerOneShot),
			Drives:    resolvedDriveIDs(o.cfg.Drives),
		})
		return
	}

	writeJSON(w, http.StatusConflict, controlMutationResponse{
		Status:  "busy",
		Message: "a foreground sync is currently running",
	})
}

func parseControlCommand(r *http.Request) (controlCommand, bool) {
	if cmd, ok := parseRootControlCommand(r); ok {
		return cmd, true
	}

	return parseDriveControlCommand(r)
}

func parseRootControlCommand(r *http.Request) (controlCommand, bool) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == controlStatusPath:
		return controlCommand{kind: controlCommandStatus}, true
	case r.Method == http.MethodPost && r.URL.Path == "/v1/reload":
		return controlCommand{kind: controlCommandReload}, true
	case r.Method == http.MethodPost && r.URL.Path == "/v1/stop":
		return controlCommand{kind: controlCommandStop}, true
	default:
		return controlCommand{}, false
	}
}

func parseDriveControlCommand(r *http.Request) (controlCommand, bool) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 4 || parts[0] != "v1" || parts[1] != "drives" || r.Method != http.MethodPost {
		return controlCommand{}, false
	}

	rawDrive, err := url.PathUnescape(parts[2])
	if err != nil {
		return controlCommand{}, false
	}
	cid, err := driveid.NewCanonicalID(rawDrive)
	if err != nil {
		return controlCommand{}, false
	}

	if len(parts) == 5 && parts[3] == "held-deletes" && parts[4] == "approve" {
		return controlCommand{kind: controlCommandApproveHeldDeletes, driveID: cid}, true
	}

	return parseConflictResolutionCommand(r, parts, cid)
}

func parseConflictResolutionCommand(
	r *http.Request,
	parts []string,
	cid driveid.CanonicalID,
) (controlCommand, bool) {
	if len(parts) != 6 || parts[3] != "conflicts" || parts[5] != "resolution-request" {
		return controlCommand{}, false
	}

	conflictID, err := url.PathUnescape(parts[4])
	if err != nil {
		return controlCommand{}, false
	}
	var body controlConflictRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return controlCommand{}, false
	}

	return controlCommand{
		kind:       controlCommandRequestConflictResolution,
		driveID:    cid,
		conflictID: conflictID,
		resolution: body.Resolution,
	}, true
}

func writeControlResponse(w http.ResponseWriter, response controlResponse) {
	if response.Err != nil {
		status := response.StatusCode
		if status == 0 {
			status = http.StatusInternalServerError
		}
		writeJSON(w, status, controlMutationResponse{
			Status:  controlStatusError,
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

func resolvedDriveIDs(drives []*config.ResolvedDrive) []string {
	ids := make([]string, 0, len(drives))
	for _, drive := range drives {
		ids = append(ids, drive.CanonicalID.String())
	}
	return ids
}

func (o *Orchestrator) handleControlCommand(
	ctx context.Context,
	cmd *controlCommand,
	mode synctypes.SyncMode,
	opts synctypes.WatchOpts,
	runners map[driveid.CanonicalID]*watchRunner,
) bool {
	switch cmd.kind {
	case controlCommandStatus:
		cmd.response <- controlResponse{Body: controlStatusResponse{
			OwnerMode: string(controlOwnerWatch),
			Drives:    resolvedDriveIDs(o.cfg.Drives),
		}}
	case controlCommandReload:
		o.reload(ctx, mode, opts, runners)
		cmd.response <- controlResponse{Body: controlMutationResponse{Status: "reloaded"}}
	case controlCommandStop:
		cmd.response <- controlResponse{Body: controlMutationResponse{Status: "stopping"}}
		return true
	case controlCommandApproveHeldDeletes:
		err := o.approveHeldDeletes(ctx, cmd.driveID)
		if err == nil {
			o.wakeRunner(runners, cmd.driveID)
		}
		cmd.response <- controlResponse{Err: err, Body: controlMutationResponse{Status: "approved"}}
	case controlCommandRequestConflictResolution:
		result, err := o.requestConflictResolution(ctx, cmd.driveID, cmd.conflictID, cmd.resolution)
		if err == nil {
			o.wakeRunner(runners, cmd.driveID)
		}
		cmd.response <- controlResponse{Err: err, Body: controlMutationResponse{Status: string(result.Status)}}
	}

	return false
}

func (o *Orchestrator) approveHeldDeletes(ctx context.Context, cid driveid.CanonicalID) error {
	store, err := o.openDriveStore(ctx, cid)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := store.Close(ctx); closeErr != nil {
			o.logger.Warn("close sync store after held-delete approval",
				slog.String("drive", cid.String()),
				slog.String("error", closeErr.Error()),
			)
		}
	}()

	if err := store.ApproveHeldDeletes(ctx); err != nil {
		return fmt.Errorf("approve held deletes for %s: %w", cid.String(), err)
	}

	return nil
}

func (o *Orchestrator) requestConflictResolution(
	ctx context.Context,
	cid driveid.CanonicalID,
	conflictID string,
	resolution string,
) (syncstore.ConflictRequestResult, error) {
	store, err := o.openDriveStore(ctx, cid)
	if err != nil {
		return syncstore.ConflictRequestResult{}, err
	}
	defer func() {
		if closeErr := store.Close(ctx); closeErr != nil {
			o.logger.Warn("close sync store after conflict request",
				slog.String("drive", cid.String()),
				slog.String("error", closeErr.Error()),
			)
		}
	}()

	result, err := store.RequestConflictResolution(ctx, conflictID, resolution)
	if err != nil {
		return syncstore.ConflictRequestResult{}, fmt.Errorf("request conflict resolution for %s: %w", cid.String(), err)
	}

	return result, nil
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

func (o *Orchestrator) openDriveStore(ctx context.Context, cid driveid.CanonicalID) (*syncstore.SyncStore, error) {
	for _, rd := range o.cfg.Drives {
		if !rd.CanonicalID.Equal(cid) {
			continue
		}
		dbPath := rd.StatePath()
		if dbPath == "" {
			return nil, fmt.Errorf("cannot determine state DB path for drive %q", cid.String())
		}
		store, err := syncstore.NewSyncStore(ctx, dbPath, o.logger)
		if err != nil {
			return nil, fmt.Errorf("open sync store for %s: %w", cid.String(), err)
		}
		return store, nil
	}

	return nil, fmt.Errorf("drive %q is not managed by this sync process", cid.String())
}

func (o *Orchestrator) wakeRunner(runners map[driveid.CanonicalID]*watchRunner, cid driveid.CanonicalID) {
	wr := runners[cid]
	if wr == nil || wr.userIntentWake == nil {
		return
	}
	select {
	case wr.userIntentWake <- struct{}{}:
	default:
	}
}
