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

	pages := newAskedPages("drive item search", c.maxListPages)
	for path != "" {
		if _, err := pages.ask(path); err != nil {
			return nil, err
		}

		pageItems, nextPath, err := c.fetchSearchPage(ctx, path)
		if err != nil {
			return nil, err
		}

		items = append(items, pageItems...)
		path = nextPath
	}

	c.logger.Info("search returned items", slog.Int("count", len(items)))

	return items, nil
}

// fetchSearchPage fetches one page of a drive item search: it decodes the
// response, normalizes the items and extracts the @odata.nextLink.
func (c *Client) fetchSearchPage(ctx context.Context, path string) ([]Item, string, error) {
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close() //nolint:errcheck // response body is read-only; its close reports nothing a caller can act on

	var page struct {
		Value    []driveItemResponse `json:"value"`
		NextLink string              `json:"@odata.nextLink"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, "", fmt.Errorf("graph: decoding search response: %w", err)
	}

	items := make([]Item, 0, len(page.Value))
	for i := range page.Value {
		items = append(items, page.Value[i].toItem(c.logger))
	}

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
