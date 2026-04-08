package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
)

// SearchDriveItems searches the user's drive using GET /me/drive/root/search(q='{query}').
// Returns items including shared content (items with remoteItem facets).
// Identity data in search results is incomplete (no email); callers should
// enrich shared items via GetItem if owner email is needed.
func (c *Client) SearchDriveItems(ctx context.Context, query string) ([]Item, error) {
	c.logger.Info("searching drive items", slog.String("query", query))

	var items []Item
	path := fmt.Sprintf("/me/drive/root/search(q='%s')", url.QueryEscape(query))

	for path != "" {
		page, nextPath, err := c.fetchItemPage(ctx, path, "search")
		if err != nil {
			return nil, err
		}

		items = append(items, page...)
		path = nextPath
	}

	c.logger.Info("search returned items", slog.Int("count", len(items)))

	return items, nil
}

// fetchItemPage fetches one page of items from a paginated endpoint.
// Handles response decoding, item normalization, and @odata.nextLink
// extraction. The label is used in error messages only.
func (c *Client) fetchItemPage(ctx context.Context, path, label string) ([]Item, string, error) {
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	var page struct {
		Value    []driveItemResponse `json:"value"`
		NextLink string              `json:"@odata.nextLink"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, "", fmt.Errorf("graph: decoding %s response: %w", label, err)
	}

	items := make([]Item, 0, len(page.Value))
	for i := range page.Value {
		items = append(items, page.Value[i].toItem(c.logger))
	}
	items = normalizeListedItems(items, c.logger)

	var nextPath string
	if page.NextLink != "" {
		var stripErr error

		nextPath, stripErr = c.stripBaseURL(page.NextLink)
		if stripErr != nil {
			return nil, "", stripErr
		}
	}

	return items, nextPath, nil
}
