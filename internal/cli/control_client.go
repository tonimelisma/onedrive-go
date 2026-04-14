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
)

type controlOwnerState string

const (
	controlOwnerStateWatchOwner      controlOwnerState = "watch_owner"
	controlOwnerStateOneShotOwner    controlOwnerState = "oneshot_owner"
	controlOwnerStateNoSocket        controlOwnerState = "no_socket"
	controlOwnerStatePathUnavailable controlOwnerState = "path_unavailable"
	controlOwnerStateProbeFailed     controlOwnerState = "probe_failed"
)

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

		return controlOwnerProbe{state: controlOwnerStateProbeFailed}, fmt.Errorf("send control status request: %w", err)
	}
	statusCode := resp.StatusCode
	var decoded synccontrol.StatusResponse
	decodeErr := json.NewDecoder(resp.Body).Decode(&decoded)
	closeErr := resp.Body.Close()
	if closeErr != nil {
		return controlOwnerProbe{state: controlOwnerStateProbeFailed}, fmt.Errorf("close control status response: %w", closeErr)
	}
	if statusCode != http.StatusOK {
		return controlOwnerProbe{state: controlOwnerStateProbeFailed}, fmt.Errorf("unexpected control status response: %s", resp.Status)
	}
	if decodeErr != nil {
		return controlOwnerProbe{state: controlOwnerStateProbeFailed}, fmt.Errorf("decode control status response: %w", decodeErr)
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

func (c *controlSocketClient) reload(ctx context.Context) error {
	if c.ownerMode() != synccontrol.OwnerModeWatch {
		return nil
	}

	response, err := c.postJSON(ctx, synccontrol.PathReload, nil)
	if err != nil {
		return err
	}
	if response.Message != "" && response.Status == synccontrol.StatusError {
		return &controlDaemonError{
			statusCode: http.StatusInternalServerError,
			code:       response.Code,
			message:    response.Message,
		}
	}
	return nil
}

func (c *controlSocketClient) postJSON(ctx context.Context, path string, body any) (synccontrol.MutationResponse, error) {
	var decoded synccontrol.MutationResponse
	if err := c.postJSONInto(ctx, path, body, controlClientTimeout, &decoded); err != nil {
		return synccontrol.MutationResponse{}, err
	}

	return decoded, nil
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

func decodeControlJSONResponse(resp *http.Response, target any) error {
	if resp.StatusCode >= http.StatusBadRequest {
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
