package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
)

// SearchSites searches for SharePoint sites matching a query string and
// returns at most limit of them.
// Uses GET /sites?search={query}&$top={limit}&$select=id,displayName,name,webUrl.
// Graph treats $top as a page size and may continue with @odata.nextLink, so
// the walk follows the continuation until limit sites are in hand or the
// pages run out. Personal accounts have no SharePoint — the caller should
// only call this for business accounts.
func (c *Client) SearchSites(ctx context.Context, query string, limit int) ([]Site, error) {
	c.logger.Info("searching SharePoint sites", slog.String("query", query), slog.Int("limit", limit))

	path := fmt.Sprintf("/sites?search=%s&$top=%d&$select=id,displayName,name,webUrl", url.QueryEscape(query), limit)

	sites := make([]Site, 0)

	for page := 1; path != "" && len(sites) < limit; page++ {
		if page > defaultMaxListPages {
			return nil, fmt.Errorf("graph: site search exceeded %d pages", defaultMaxListPages)
		}

		slr, err := c.fetchSitesPage(ctx, path)
		if err != nil {
			return nil, err
		}

		for _, sr := range slr.Value {
			sites = append(sites, sr.toSite())
		}

		path = ""
		if slr.NextLink != "" {
			// Graph returns @odata.nextLink as an absolute URL, while c.do
			// prepends the base URL, so the link must be made relative first;
			// stripBaseURL also refuses a continuation off the Graph host.
			path, err = c.stripBaseURL(slr.NextLink)
			if err != nil {
				return nil, err
			}
		}
	}

	if len(sites) > limit {
		sites = sites[:limit]
	}

	c.logger.Info("found SharePoint sites", slog.Int("count", len(sites)))

	return sites, nil
}

func (c *Client) fetchSitesPage(ctx context.Context, path string) (*sitesListResponse, error) {
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // response body is read-only; its close reports nothing a caller can act on

	var slr sitesListResponse
	if err := json.NewDecoder(resp.Body).Decode(&slr); err != nil {
		return nil, fmt.Errorf("graph: decoding sites response: %w", err)
	}

	return &slr, nil
}

// SiteDrives lists document libraries (drives) for a SharePoint site.
// Uses GET /sites/{siteID}/drives?$select=id,name,driveType,quota.
func (c *Client) SiteDrives(ctx context.Context, siteID string) ([]Drive, error) {
	c.logger.Debug("listing site drives", slog.String("site_id", siteID))

	path := fmt.Sprintf("/sites/%s/drives?$select=id,name,driveType,quota", siteID)

	drives, err := c.fetchAllDrives(ctx, path, "site drives")
	if err != nil {
		return nil, err
	}

	c.logger.Debug("listed site drives", slog.String("site_id", siteID), slog.Int("count", len(drives)))

	return drives, nil
}
