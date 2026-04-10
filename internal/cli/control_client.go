package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synccontrol"
)

const controlClientTimeout = 2 * time.Second

type controlSocketClient struct {
	transport *http.Transport
	status    synccontrol.StatusResponse
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

func isControlDaemonError(err error) bool {
	var daemonErr *controlDaemonError
	return errors.As(err, &daemonErr)
}

func openControlSocketClient(ctx context.Context) (*controlSocketClient, bool) {
	socketPath := config.ControlSocketPath()
	if socketPath == "" {
		return nil, false
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	client := &controlSocketClient{
		transport: transport,
	}

	requestCtx, cancel := context.WithTimeout(ctx, controlClientTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, synccontrol.HTTPBaseURL+synccontrol.PathStatus, http.NoBody)
	if err != nil {
		return nil, false
	}
	resp, err := client.transport.RoundTrip(req)
	if err != nil {
		return nil, false
	}
	statusCode := resp.StatusCode
	var decoded synccontrol.StatusResponse
	decodeErr := json.NewDecoder(resp.Body).Decode(&decoded)
	closeErr := resp.Body.Close()
	if closeErr != nil {
		return nil, false
	}
	if statusCode != http.StatusOK {
		return nil, false
	}
	if decodeErr != nil {
		return nil, false
	}
	if decoded.OwnerMode == "" {
		return nil, false
	}

	client.status = decoded
	return client, true
}

func (c *controlSocketClient) ownerMode() synccontrol.OwnerMode {
	return c.status.OwnerMode
}

func (c *controlSocketClient) approveHeldDeletes(ctx context.Context, cid driveid.CanonicalID) error {
	path := synccontrol.HeldDeletesApprovePath(cid.String())
	response, err := c.postJSON(ctx, path, nil)
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

func (c *controlSocketClient) requestConflictResolution(
	ctx context.Context,
	cid driveid.CanonicalID,
	conflictID string,
	resolution string,
) (string, error) {
	path := synccontrol.ConflictResolutionRequestPath(cid.String(), conflictID)
	response, err := c.postJSON(ctx, path, synccontrol.ConflictResolutionRequest{Resolution: resolution})
	if err != nil {
		return "", err
	}
	if response.Message != "" && response.Status == synccontrol.StatusError {
		return "", &controlDaemonError{
			statusCode: http.StatusInternalServerError,
			code:       response.Code,
			message:    response.Message,
		}
	}
	return string(response.Status), nil
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
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return synccontrol.MutationResponse{}, fmt.Errorf("encode control request: %w", err)
		}
		payload = encoded
	}

	requestCtx, cancel := context.WithTimeout(ctx, controlClientTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, synccontrol.HTTPBaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return synccontrol.MutationResponse{}, fmt.Errorf("build control request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.transport.RoundTrip(req)
	if err != nil {
		return synccontrol.MutationResponse{}, fmt.Errorf("send control request: %w", err)
	}

	var decoded synccontrol.MutationResponse
	decodeErr := json.NewDecoder(resp.Body).Decode(&decoded)
	closeErr := resp.Body.Close()
	if decodeErr != nil {
		return synccontrol.MutationResponse{}, &controlDaemonError{
			statusCode: resp.StatusCode,
			code:       synccontrol.ErrorInternal,
			message:    fmt.Sprintf("decode control response: %v", decodeErr),
		}
	}
	if closeErr != nil {
		return synccontrol.MutationResponse{}, &controlDaemonError{
			statusCode: resp.StatusCode,
			code:       synccontrol.ErrorInternal,
			message:    fmt.Sprintf("close control response: %v", closeErr),
		}
	}
	if resp.StatusCode >= http.StatusBadRequest {
		if decoded.Message == "" {
			decoded.Message = resp.Status
		}
		return decoded, &controlDaemonError{
			statusCode: resp.StatusCode,
			code:       decoded.Code,
			message:    decoded.Message,
		}
	}

	return decoded, nil
}
