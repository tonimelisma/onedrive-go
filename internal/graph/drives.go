package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
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
		ID:        d.ID,
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
func (c *Client) Drive(ctx context.Context, driveID string) (*Drive, error) {
	c.logger.Info("fetching drive",
		slog.String("drive_id", driveID),
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
		slog.String("id", drive.ID),
		slog.String("name", drive.Name),
		slog.String("drive_type", drive.DriveType),
	)

	return &drive, nil
}
