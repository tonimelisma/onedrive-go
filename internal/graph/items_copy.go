package graph

import (
	"context"
	"encoding/json"
	"errors"
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
	c.logger.Debug("copying item",
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
		name:   QuirkCopyDestinationTransient404,
		policy: c.policies.copyDestination,
		match:  isTransientCopyDestinationError,
	}, func() (*http.Response, error) {
		return c.do(ctx, http.MethodPost, apiPath, bodyBytes)
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // response body is read-only; its close reports nothing a caller can act on

	if _, drainErr := io.Copy(io.Discard, resp.Body); drainErr != nil {
		return nil, fmt.Errorf("graph: draining copy response: %w", drainErr)
	}

	monitorURL := resp.Header.Get("Location")
	if monitorURL == "" {
		return nil, errors.New("graph: copy response missing Location header")
	}

	validatedMonitorURL, err := c.validatedCopyMonitorURL(monitorURL)
	if err != nil {
		return nil, err
	}

	return &CopyResult{MonitorURL: validatedMonitorURL}, nil
}

// PollCopyStatus checks the progress of an async copy operation.
// The monitor URL is a pre-authenticated Azure URL — no auth header needed.
// It goes out through doPreAuth like every pre-authenticated request, so the
// retrying transport logs it by name only (the URL carries a credential,
// R-6.8.17) and it carries the User-Agent (R-6.8.18). A non-2xx monitor reply
// (expired monitor, 404, 5xx) is a *GraphError, not a status document:
// decoding it produced an empty Status and hid the HTTP failure.
func (c *Client) PollCopyStatus(ctx context.Context, monitorURL string) (*CopyStatus, error) {
	validatedMonitorURL, err := c.validatedCopyMonitorURL(monitorURL)
	if err != nil {
		return nil, err
	}

	resp, err := c.doPreAuth(ctx, "copy status", func() (*http.Request, error) {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, validatedMonitorURL, http.NoBody)
		if reqErr != nil {
			return nil, fmt.Errorf("graph: creating copy status request: %w", reqErr)
		}

		req.Header.Set("User-Agent", c.userAgent)

		return req, nil
	})
	if err != nil {
		return nil, fmt.Errorf("graph: polling copy status: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body is read-only; its close reports nothing a caller can act on

	var status CopyStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("graph: decoding copy status: %w", err)
	}

	return &status, nil
}
