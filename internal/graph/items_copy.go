package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// CopyResult holds the monitor URL returned by a copy operation.
type CopyResult struct {
	MonitorURL string
}

// CopyStatus represents the current state of an async copy operation.
type CopyStatus struct {
	Status             string  `json:"status"`
	PercentageComplete float64 `json:"percentageComplete"`
	ResourceID         string  `json:"resourceId"`
}

type copyItemRequest struct {
	ParentReference *moveParentRef `json:"parentReference"`
	Name            string         `json:"name,omitempty"`
}

// CopyItem starts an async copy of a drive item to a new location.
// Returns a CopyResult with a monitor URL. The copy completes server-side;
// poll PollCopyStatus to track progress.
func (c *Client) CopyItem(
	ctx context.Context, driveID driveid.ID, itemID, destParentID, newName string,
) (*CopyResult, error) {
	c.logger.Info("copying item",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", itemID),
		slog.String("dest_parent_id", destParentID),
		slog.String("new_name", newName),
	)

	apiPath := fmt.Sprintf("/drives/%s/items/%s/copy", driveID, itemID)

	req := copyItemRequest{
		ParentReference: &moveParentRef{ID: destParentID},
	}
	if newName != "" {
		req.Name = newName
	}

	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("graph: marshaling copy request: %w", err)
	}

	resp, err := doDocumentedGraphQuirkRetry(ctx, c, documentedGraphQuirkSpec{
		name:   "copy-destination-transient-404",
		policy: c.copyDestinationPolicy,
		match:  isTransientCopyDestinationError,
	}, func() (*http.Response, error) {
		return c.do(ctx, http.MethodPost, apiPath, bytes.NewReader(bodyBytes))
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if _, drainErr := io.Copy(io.Discard, resp.Body); drainErr != nil {
		return nil, fmt.Errorf("graph: draining copy response: %w", drainErr)
	}

	monitorURL := resp.Header.Get("Location")
	if monitorURL == "" {
		return nil, fmt.Errorf("graph: copy response missing Location header")
	}

	validatedMonitorURL, err := c.validatedCopyMonitorURL(monitorURL)
	if err != nil {
		return nil, err
	}

	return &CopyResult{MonitorURL: validatedMonitorURL}, nil
}

// PollCopyStatus checks the progress of an async copy operation.
// The monitor URL is a pre-authenticated Azure URL — no auth header needed.
func (c *Client) PollCopyStatus(ctx context.Context, monitorURL string) (*CopyStatus, error) {
	validatedMonitorURL, err := c.validatedCopyMonitorURL(monitorURL)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, validatedMonitorURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("graph: creating copy status request: %w", err)
	}

	resp, err := c.dispatchRequest(req)
	if err != nil {
		return nil, fmt.Errorf("graph: polling copy status: %w", err)
	}
	defer resp.Body.Close()

	var status CopyStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("graph: decoding copy status: %w", err)
	}

	return &status, nil
}
