package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// userResponse mirrors the Graph API /me JSON response.
// Unexported — callers use User via toUser() normalization.
type userResponse struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Mail        string `json:"mail"`
	// UPN is a fallback when mail is empty (common on Personal accounts
	// where the mail field is often blank).
	UPN string `json:"userPrincipalName"`
}

// toUser normalizes a Graph API user response into our User type.
func (u *userResponse) toUser() User {
	email := u.Mail
	if email == "" {
		email = u.UPN
	}

	return User{
		ID:          u.ID,
		DisplayName: u.DisplayName,
		Email:       email,
	}
}

// driveResponse mirrors the Graph API drive JSON response.
// Unexported — callers use Drive via toDrive() normalization.
type driveResponse struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	DriveType string      `json:"driveType"`
	Owner     *ownerFacet `json:"owner"`
	Quota     *quotaFacet `json:"quota"`
}

// ownerFacet represents the owner block in a Graph API drive response.
type ownerFacet struct {
	User struct {
		DisplayName string `json:"displayName"`
	} `json:"user"`
}

// quotaFacet represents the quota block in a Graph API drive response.
type quotaFacet struct {
	Used  int64 `json:"used"`
	Total int64 `json:"total"`
}

// drivesListResponse wraps the value array from GET /me/drives.
type drivesListResponse struct {
	Value []driveResponse `json:"value"`
}

// toDrive normalizes a Graph API drive response into our Drive type.
// Nil-safe for optional owner and quota facets.
func (d *driveResponse) toDrive() Drive {
	drive := Drive{
		ID:        driveid.New(d.ID),
		Name:      d.Name,
		DriveType: d.DriveType,
	}

	if d.Owner != nil {
		drive.OwnerName = d.Owner.User.DisplayName
	}

	if d.Quota != nil {
		drive.QuotaUsed = d.Quota.Used
		drive.QuotaTotal = d.Quota.Total
	}

	return drive
}

// siteResponse mirrors the Graph API site JSON response.
// Unexported — callers use Site via toSite() normalization.
type siteResponse struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Name        string `json:"name"`
	WebURL      string `json:"webUrl"`
}

// sitesListResponse wraps the value array from GET /sites?search=.
type sitesListResponse struct {
	Value []siteResponse `json:"value"`
}

// toSite normalizes a Graph API site response into our Site type.
func (s *siteResponse) toSite() Site {
	return Site{
		ID:          s.ID,
		DisplayName: s.DisplayName,
		Name:        s.Name,
		WebURL:      s.WebURL,
	}
}

// orgResponse mirrors the Graph API /me/organization response.
// Unexported — callers use Organization via the Organization() method.
type orgResponse struct {
	Value []orgEntry `json:"value"`
}

// orgEntry represents a single organization entry from the Graph API.
type orgEntry struct {
	DisplayName string `json:"displayName"`
}

// Me returns the authenticated user's profile.
func (c *Client) Me(ctx context.Context) (*User, error) {
	c.logger.Info("fetching authenticated user profile")

	resp, err := c.Do(ctx, http.MethodGet, "/me", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var ur userResponse
	if err := json.NewDecoder(resp.Body).Decode(&ur); err != nil {
		return nil, fmt.Errorf("graph: decoding user response: %w", err)
	}

	user := ur.toUser()

	c.logger.Debug("fetched user profile",
		slog.String("id", user.ID),
		slog.String("display_name", user.DisplayName),
	)

	return &user, nil
}

// Drives returns all drives accessible to the authenticated user.
func (c *Client) Drives(ctx context.Context) ([]Drive, error) {
	c.logger.Info("listing accessible drives")

	resp, err := c.Do(ctx, http.MethodGet, "/me/drives", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var dlr drivesListResponse
	if err := json.NewDecoder(resp.Body).Decode(&dlr); err != nil {
		return nil, fmt.Errorf("graph: decoding drives response: %w", err)
	}

	drives := make([]Drive, 0, len(dlr.Value))
	for i := range dlr.Value {
		drives = append(drives, dlr.Value[i].toDrive())
	}

	c.logger.Info("listed drives",
		slog.Int("count", len(drives)),
	)

	return drives, nil
}

// Drive returns a specific drive by ID.
func (c *Client) Drive(ctx context.Context, driveID driveid.ID) (*Drive, error) {
	c.logger.Info("fetching drive",
		slog.String("drive_id", driveID.String()),
	)

	path := fmt.Sprintf("/drives/%s", driveID)

	resp, err := c.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var dr driveResponse
	if err := json.NewDecoder(resp.Body).Decode(&dr); err != nil {
		return nil, fmt.Errorf("graph: decoding drive response: %w", err)
	}

	drive := dr.toDrive()

	c.logger.Debug("fetched drive",
		slog.String("id", drive.ID.String()),
		slog.String("name", drive.Name),
		slog.String("drive_type", drive.DriveType),
	)

	return &drive, nil
}

// SearchSites searches for SharePoint sites matching a query string.
// Uses GET /sites?search={query}&$top={limit}&$select=id,displayName,name,webUrl.
// Personal accounts have no SharePoint — the caller should only call this
// for business accounts.
func (c *Client) SearchSites(ctx context.Context, query string, limit int) ([]Site, error) {
	c.logger.Info("searching SharePoint sites", slog.String("query", query), slog.Int("limit", limit))

	path := fmt.Sprintf("/sites?search=%s&$top=%d&$select=id,displayName,name,webUrl", query, limit)

	resp, err := c.Do(ctx, http.MethodGet, path, nil)
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

	resp, err := c.Do(ctx, http.MethodGet, path, nil)
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

// Organization returns the authenticated user's organization.
// Personal accounts return an empty Organization (the API returns an empty array).
// Business/education accounts return the first organization's display name,
// which is used for sync directory naming (e.g., "~/OneDrive - Contoso").
func (c *Client) Organization(ctx context.Context) (*Organization, error) {
	c.logger.Info("fetching user organization")

	resp, err := c.Do(ctx, http.MethodGet, "/me/organization", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var orgResp orgResponse
	if err := json.NewDecoder(resp.Body).Decode(&orgResp); err != nil {
		return nil, fmt.Errorf("graph: decoding organization response: %w", err)
	}

	org := &Organization{}
	if len(orgResp.Value) > 0 {
		org.DisplayName = orgResp.Value[0].DisplayName
	}

	c.logger.Debug("fetched organization",
		slog.String("display_name", org.DisplayName),
		slog.Bool("has_org", len(orgResp.Value) > 0),
	)

	return org, nil
}
