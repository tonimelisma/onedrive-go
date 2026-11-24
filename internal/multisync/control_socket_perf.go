// The control socket's perf endpoints: the live perf status and a bounded
// perf capture.

package multisync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"

	"github.com/tonimelisma/onedrive-go/internal/perf"
)

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
		return perf.CaptureOptions{}, errors.New("missing perf capture request")
	}

	if request.DurationMS <= 0 {
		return perf.CaptureOptions{}, errors.New("perf capture duration must be greater than zero")
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
