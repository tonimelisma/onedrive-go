package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
)

// SearchSites searches for SharePoint sites matching a query string.
// Uses GET /sites?search={query}&$top={limit}&$select=id,displayName,name,webUrl.
// Personal accounts have no SharePoint — the caller should only call this
// for business accounts.
func (c *Client) SearchSites(ctx context.Context, query string, limit int) ([]Site, error) {
	c.logger.Info("searching SharePoint sites", slog.String("query", query), slog.Int("limit", limit))

	path := fmt.Sprintf("/sites?search=%s&$top=%d&$select=id,displayName,name,webUrl", url.QueryEscape(query), limit)

	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var slr sitesListResponse
	if err := json.NewDecoder(resp.Body).Decode(&slr); err != nil {
		return nil, fmt.Errorf("graph: decoding sites response: %w", err)
	}

	sites := make([]Site, 0, len(slr.Value))
	for _, sr := range slr.Value {
		sites = append(sites, sr.toSite())
	}

	c.logger.Info("found SharePoint sites", slog.Int("count", len(sites)))

	return sites, nil
}

// SiteDrives lists document libraries (drives) for a SharePoint site.
// Uses GET /sites/{siteID}/drives?$select=id,name,driveType,quota.
func (c *Client) SiteDrives(ctx context.Context, siteID string) ([]Drive, error) {
	c.logger.Debug("listing site drives", slog.String("site_id", siteID))

	path := fmt.Sprintf("/sites/%s/drives?$select=id,name,driveType,quota", siteID)

	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var dlr drivesListResponse
	if err := json.NewDecoder(resp.Body).Decode(&dlr); err != nil {
		return nil, fmt.Errorf("graph: decoding site drives response: %w", err)
	}

	drives := make([]Drive, 0, len(dlr.Value))
	for i := range dlr.Value {
		drives = append(drives, dlr.Value[i].toDrive())
	}

	c.logger.Debug("listed site drives", slog.String("site_id", siteID), slog.Int("count", len(drives)))

	return drives, nil
}
