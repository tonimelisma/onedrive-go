package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const (
	controlClientTimeout = 2 * time.Second
	controlStatusError   = "error"
)

type controlSocketClient struct {
	transport *http.Transport
}

type controlMutationResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

type controlConflictRequestBody struct {
	Resolution string `json:"resolution"`
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

	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "http://unix/v1/status", http.NoBody)
	if err != nil {
		return nil, false
	}
	resp, err := client.transport.RoundTrip(req)
	if err != nil {
		return nil, false
	}
	statusCode := resp.StatusCode
	if closeErr := resp.Body.Close(); closeErr != nil {
		return nil, false
	}
	if statusCode != http.StatusOK {
		return nil, false
	}

	return client, true
}

func (c *controlSocketClient) approveHeldDeletes(ctx context.Context, cid driveid.CanonicalID) error {
	path := "/v1/drives/" + url.PathEscape(cid.String()) + "/held-deletes/approve"
	response, err := c.postJSON(ctx, path, nil)
	if err != nil {
		return err
	}
	if response.Message != "" && response.Status == controlStatusError {
		return errors.New(response.Message)
	}
	return nil
}

func (c *controlSocketClient) requestConflictResolution(
	ctx context.Context,
	cid driveid.CanonicalID,
	conflictID string,
	resolution string,
) (string, error) {
	path := "/v1/drives/" + url.PathEscape(cid.String()) +
		"/conflicts/" + url.PathEscape(conflictID) + "/resolution-request"
	response, err := c.postJSON(ctx, path, controlConflictRequestBody{Resolution: resolution})
	if err != nil {
		return "", err
	}
	if response.Message != "" && response.Status == controlStatusError {
		return "", errors.New(response.Message)
	}
	return response.Status, nil
}

func (c *controlSocketClient) reload(ctx context.Context) error {
	response, err := c.postJSON(ctx, "/v1/reload", nil)
	if err != nil {
		return err
	}
	if response.Message != "" && response.Status == controlStatusError {
		return errors.New(response.Message)
	}
	return nil
}

func (c *controlSocketClient) postJSON(ctx context.Context, path string, body any) (controlMutationResponse, error) {
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return controlMutationResponse{}, fmt.Errorf("encode control request: %w", err)
		}
		payload = encoded
	}

	requestCtx, cancel := context.WithTimeout(ctx, controlClientTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, "http://unix"+path, bytes.NewReader(payload))
	if err != nil {
		return controlMutationResponse{}, fmt.Errorf("build control request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.transport.RoundTrip(req)
	if err != nil {
		return controlMutationResponse{}, fmt.Errorf("send control request: %w", err)
	}

	var decoded controlMutationResponse
	decodeErr := json.NewDecoder(resp.Body).Decode(&decoded)
	closeErr := resp.Body.Close()
	if decodeErr != nil {
		return controlMutationResponse{}, fmt.Errorf("decode control response: %w", decodeErr)
	}
	if closeErr != nil {
		return controlMutationResponse{}, fmt.Errorf("close control response: %w", closeErr)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		if decoded.Message == "" {
			decoded.Message = resp.Status
		}
		return decoded, errors.New(decoded.Message)
	}

	return decoded, nil
}
