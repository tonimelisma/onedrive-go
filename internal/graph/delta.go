package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// deltaPreferHeader requests that the Graph API include remote/shared items
// using stable alias IDs in delta responses. Without this header, Personal
// accounts may receive incomplete delta results for shared folders.
// See docs/tier1-research/issues-graph-api-bugs.md.
var deltaPreferHeader = http.Header{
	"Prefer": {"deltashowremoteitemsaliasid"},
}

// deltaResponse mirrors the Graph API delta response JSON structure.
// Unexported — callers receive normalized DeltaPage values.
type deltaResponse struct {
	Value     []driveItemResponse `json:"value"`
	NextLink  string              `json:"@odata.nextLink"`  //nolint:tagliatelle // OData annotation key
	DeltaLink string              `json:"@odata.deltaLink"` //nolint:tagliatelle // OData annotation key
}

// deltaHTTPPrefix is the scheme prefix used to detect full URL tokens
// returned by the Graph API delta endpoint.
const deltaHTTPPrefix = "http"

// Delta fetches one page of delta changes for a drive.
// Pass an empty token for the initial sync (fetches all items).
// For subsequent calls, pass the DeltaLink or NextLink value from the
// previous DeltaPage — these are full URLs that get converted to paths.
// Returns a DeltaPage with normalized items, and either NextLink (more pages)
// or DeltaLink (done). HTTP 410 (Gone) means the token has expired — returns ErrGone.
func (c *Client) Delta(ctx context.Context, driveID, token string) (*DeltaPage, error) {
	path, err := c.buildDeltaPath(driveID, token)
	if err != nil {
		return nil, err
	}

	c.logger.Info("fetching delta page",
		slog.String("drive_id", driveID),
		slog.Bool("initial_sync", token == ""),
	)

	resp, err := c.DoWithHeaders(ctx, http.MethodGet, path, nil, deltaPreferHeader)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var dr deltaResponse
	if err := json.NewDecoder(resp.Body).Decode(&dr); err != nil {
		return nil, fmt.Errorf("graph: decoding delta response: %w", err)
	}

	items := make([]Item, 0, len(dr.Value))
	for i := range dr.Value {
		items = append(items, dr.Value[i].toItem(c.logger))
	}

	// Apply delta-specific normalization pipeline (package filtering,
	// hash clearing, dedup, deletion reordering).
	items = normalizeDeltaItems(items, c.logger)

	c.logger.Debug("fetched delta page",
		slog.Int("raw_count", len(dr.Value)),
		slog.Int("normalized_count", len(items)),
		slog.Bool("has_next_link", dr.NextLink != ""),
		slog.Bool("has_delta_link", dr.DeltaLink != ""),
	)

	return &DeltaPage{
		Items:     items,
		NextLink:  dr.NextLink,
		DeltaLink: dr.DeltaLink,
	}, nil
}

// buildDeltaPath constructs the API path for a delta request.
// Empty token means initial sync; non-empty token is a full URL from a
// previous response that gets stripped to a relative path.
func (c *Client) buildDeltaPath(driveID, token string) (string, error) {
	if token == "" || !strings.HasPrefix(token, deltaHTTPPrefix) {
		return fmt.Sprintf("/drives/%s/root/delta", driveID), nil
	}

	path, err := c.stripBaseURL(token)
	if err != nil {
		return "", fmt.Errorf("graph: invalid delta token URL: %w", err)
	}

	return path, nil
}

// DeltaAll fetches all pages of delta changes and returns the combined items
// and the new delta token for the next sync cycle.
// On success, the returned token is always a non-empty DeltaLink.
func (c *Client) DeltaAll(ctx context.Context, driveID, token string) ([]Item, string, error) {
	c.logger.Info("starting full delta enumeration",
		slog.String("drive_id", driveID),
		slog.Bool("initial_sync", token == ""),
	)

	var allItems []Item

	currentToken := token
	page := 1

	for {
		dp, err := c.Delta(ctx, driveID, currentToken)
		if err != nil {
			return nil, "", err
		}

		allItems = append(allItems, dp.Items...)

		c.logger.Debug("accumulated delta items",
			slog.Int("page", page),
			slog.Int("page_items", len(dp.Items)),
			slog.Int("total_items", len(allItems)),
		)

		// DeltaLink means we have consumed all pages — done.
		if dp.DeltaLink != "" {
			c.logger.Info("full delta enumeration complete",
				slog.String("drive_id", driveID),
				slog.Int("total_items", len(allItems)),
				slog.Int("pages", page),
			)

			return allItems, dp.DeltaLink, nil
		}

		// NextLink means more pages — continue with the next page URL as token.
		if dp.NextLink != "" {
			currentToken = dp.NextLink
			page++

			continue
		}

		// Neither link present — unexpected, but treat as complete with empty token.
		c.logger.Warn("delta response has neither nextLink nor deltaLink",
			slog.String("drive_id", driveID),
			slog.Int("page", page),
		)

		return allItems, "", nil
	}
}
