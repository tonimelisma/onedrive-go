package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"

	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

const (
	controlClientTimeout        = 2 * time.Second
	controlCaptureTimeoutBuffer = 10 * time.Second
	// controlReloadClientMargin covers connecting and scheduling on top of
	// the owner's own waits.
	controlReloadClientMargin = 2 * time.Second
	// controlReloadClientTimeout outlasts every wait the watch owner may
	// spend before it answers a reload, so the client hears busy or
	// reload_in_progress instead of giving up first and guessing.
	controlReloadClientTimeout = synccontrol.LoopAcceptTimeout + synccontrol.ReloadAnswerTimeout +
		synccontrol.ReplyWriteTimeout + controlReloadClientMargin
)

type controlOwnerState string

const (
	controlOwnerStateWatchOwner      controlOwnerState = "watch_owner"
	controlOwnerStateOneShotOwner    controlOwnerState = "oneshot_owner"
	controlOwnerStateNoSocket        controlOwnerState = "no_socket"
	controlOwnerStatePathUnavailable controlOwnerState = "path_unavailable"
	controlOwnerStateProbeFailed     controlOwnerState = "probe_failed"
	// controlOwnerStateUnresponsive is an owner that holds the socket but did
	// not answer the status probe in time: it said busy, or the client's
	// deadline passed. Only a watch owner routes status through a loop that
	// can be busy; a one-shot owner answers status in the handler at once.
	controlOwnerStateUnresponsive controlOwnerState = "unresponsive"
)

// controlOwnerDidNotAnswer reports whether err means a live owner did not
// answer in time: it answered busy, or a deadline passed while it was
// working. Neither means the owner is gone.
func controlOwnerDidNotAnswer(err error) bool {
	var daemonErr *controlDaemonError
	if errors.As(err, &daemonErr) && daemonErr.code == synccontrol.ErrorBusy {
		return true
	}

	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded)
}

type controlSocketClient struct {
	transport *http.Transport
	status    synccontrol.StatusResponse
}

type controlOwnerProbe struct {
	state  controlOwnerState
	client *controlSocketClient
}

type controlDaemonError struct {
	statusCode int
	code       synccontrol.ErrorCode
	message    string
}

func (e *controlDaemonError) Error() string {
	if e.code != "" && e.message != "" {
		return fmt.Sprintf("%s: %s", e.code, e.message)
	}
	if e.code != "" {
		return string(e.code)
	}
	return e.message
}

func probeControlOwner(ctx context.Context) (controlOwnerProbe, error) {
	socketPath, err := config.ControlSocketPath()
	if err != nil {
		return controlOwnerProbe{state: controlOwnerStatePathUnavailable}, fmt.Errorf("control socket path: %w", err)
	}
	transport := newControlSocketTransport(socketPath)
	client := &controlSocketClient{
		transport: transport,
	}

	requestCtx, cancel := context.WithTimeout(ctx, controlClientTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, synccontrol.HTTPBaseURL+synccontrol.PathStatus, http.NoBody)
	if err != nil {
		return controlOwnerProbe{state: controlOwnerStateProbeFailed}, fmt.Errorf("build control status request: %w", err)
	}
	resp, err := client.transport.RoundTrip(req)
	if err != nil {
		if isControlSocketUnavailable(err) {
			return controlOwnerProbe{state: controlOwnerStateNoSocket}, nil
		}

		return controlOwnerProbe{state: probeFailureState(err)}, fmt.Errorf("send control status request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		answerErr := readControlErrorResponse(resp)
		return controlOwnerProbe{state: probeFailureState(answerErr)},
			fmt.Errorf("unexpected control status response (%s): %w", resp.Status, answerErr)
	}
	var decoded synccontrol.StatusResponse
	decodeErr := json.NewDecoder(resp.Body).Decode(&decoded)
	if closeErr := resp.Body.Close(); closeErr != nil {
		return controlOwnerProbe{state: probeFailureState(closeErr)}, fmt.Errorf("close control status response: %w", closeErr)
	}
	if decodeErr != nil {
		return controlOwnerProbe{state: probeFailureState(decodeErr)}, fmt.Errorf("decode control status response: %w", decodeErr)
	}
	if decoded.OwnerMode == "" {
		return controlOwnerProbe{state: controlOwnerStateProbeFailed}, fmt.Errorf("decode control status response: missing owner mode")
	}

	client.status = decoded
	switch decoded.OwnerMode {
	case synccontrol.OwnerModeWatch:
		return controlOwnerProbe{
			state:  controlOwnerStateWatchOwner,
			client: client,
		}, nil
	case synccontrol.OwnerModeOneShot:
		return controlOwnerProbe{
			state:  controlOwnerStateOneShotOwner,
			client: client,
		}, nil
	default:
		return controlOwnerProbe{state: controlOwnerStateProbeFailed}, fmt.Errorf(
			"decode control status response: unknown owner mode %q",
			decoded.OwnerMode,
		)
	}
}

// probeFailureState classifies a status probe that reached a socket but got
// no usable answer: a live owner that did not answer in time, or a failure
// the probe cannot read further into.
func probeFailureState(err error) controlOwnerState {
	if controlOwnerDidNotAnswer(err) {
		return controlOwnerStateUnresponsive
	}

	return controlOwnerStateProbeFailed
}

func newControlSocketTransport(socketPath string) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
}

func isControlSocketUnavailable(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}

func isControlSocketGone(err error) bool {
	return isControlSocketUnavailable(err) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET)
}

func (c *controlSocketClient) ownerMode() synccontrol.OwnerMode {
	return c.status.OwnerMode
}

func (c *controlSocketClient) perfStatus(ctx context.Context) (synccontrol.PerfStatusResponse, error) {
	var response synccontrol.PerfStatusResponse
	if err := c.getJSON(ctx, synccontrol.PathPerfStatus, controlClientTimeout, &response); err != nil {
		return synccontrol.PerfStatusResponse{}, err
	}

	return response, nil
}

func (c *controlSocketClient) capturePerf(
	ctx context.Context,
	request synccontrol.PerfCaptureRequest,
) (synccontrol.PerfCaptureResponse, error) {
	timeout := time.Duration(request.DurationMS)*time.Millisecond + controlCaptureTimeoutBuffer
	if timeout < controlClientTimeout {
		timeout = controlClientTimeout
	}

	var response synccontrol.PerfCaptureResponse
	if err := c.postJSONInto(ctx, synccontrol.PathPerfCapture, request, timeout, &response); err != nil {
		return synccontrol.PerfCaptureResponse{}, err
	}

	return response, nil
}

// reload asks a watch owner to apply the config on disk and returns how far
// it got: synccontrol.StatusReloaded when the change is applied, or
// synccontrol.StatusReloadInProgress when the owner took the reload but was
// still applying it when its answer was due. A busy owner and a reload the
// owner could not apply are errors (controlDaemonError).
func (c *controlSocketClient) reload(ctx context.Context) (synccontrol.Status, error) {
	var response synccontrol.MutationResponse
	if err := c.postJSONInto(ctx, synccontrol.PathReload, nil, controlReloadClientTimeout, &response); err != nil {
		return "", err
	}

	return response.Status, nil
}

func (c *controlSocketClient) getJSON(ctx context.Context, path string, timeout time.Duration, target any) error {
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, synccontrol.HTTPBaseURL+path, http.NoBody)
	if err != nil {
		return fmt.Errorf("build control request: %w", err)
	}

	resp, err := c.transport.RoundTrip(req)
	if err != nil {
		return fmt.Errorf("send control request: %w", err)
	}

	return decodeControlJSONResponse(resp, target)
}

func (c *controlSocketClient) postJSONInto(
	ctx context.Context,
	path string,
	body any,
	timeout time.Duration,
	target any,
) error {
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode control request: %w", err)
		}
		payload = encoded
	}

	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, synccontrol.HTTPBaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build control request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.transport.RoundTrip(req)
	if err != nil {
		return fmt.Errorf("send control request: %w", err)
	}

	return decodeControlJSONResponse(resp, target)
}

// readControlErrorResponse reads an owner's `{status, code, message}` error
// answer, closes the body, and returns it as a controlDaemonError. An answer
// that is not that shape is an internal_error carrying what went wrong.
func readControlErrorResponse(resp *http.Response) error {
	var decoded synccontrol.MutationResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		closeErr := resp.Body.Close()
		message := fmt.Sprintf("decode control response: %v", err)
		if closeErr != nil {
			message = fmt.Sprintf("%s (close: %v)", message, closeErr)
		}
		return &controlDaemonError{
			statusCode: resp.StatusCode,
			code:       synccontrol.ErrorInternal,
			message:    message,
		}
	}
	if decoded.Message == "" {
		decoded.Message = resp.Status
	}
	if closeErr := resp.Body.Close(); closeErr != nil {
		return &controlDaemonError{
			statusCode: resp.StatusCode,
			code:       synccontrol.ErrorInternal,
			message:    fmt.Sprintf("close control response: %v", closeErr),
		}
	}

	return &controlDaemonError{
		statusCode: resp.StatusCode,
		code:       decoded.Code,
		message:    decoded.Message,
	}
}

func decodeControlJSONResponse(resp *http.Response, target any) error {
	if resp.StatusCode >= http.StatusBadRequest {
		return readControlErrorResponse(resp)
	}

	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		closeErr := resp.Body.Close()
		message := fmt.Sprintf("decode control response: %v", err)
		if closeErr != nil {
			message = fmt.Sprintf("%s (close: %v)", message, closeErr)
		}
		return &controlDaemonError{
			statusCode: resp.StatusCode,
			code:       synccontrol.ErrorInternal,
			message:    message,
		}
	}
	if closeErr := resp.Body.Close(); closeErr != nil {
		return &controlDaemonError{
			statusCode: resp.StatusCode,
			code:       synccontrol.ErrorInternal,
			message:    fmt.Sprintf("close control response: %v", closeErr),
		}
	}

	return nil
}
