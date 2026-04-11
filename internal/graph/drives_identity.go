package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Me returns the authenticated user's profile.
func (c *Client) Me(ctx context.Context) (*User, error) {
	c.logger.Info("fetching authenticated user profile")

	resp, err := c.do(ctx, http.MethodGet, "/me", nil)
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
// Retries on transient 403 from /me/drives — Microsoft Graph occasionally
// returns "accessDenied" during token propagation even with a valid token.
func (c *Client) Drives(ctx context.Context) ([]Drive, error) {
	c.logger.Info("listing accessible drives")

	return doDocumentedGraphQuirkRetry(ctx, c, documentedGraphQuirkSpec{
		name:   "drives-token-propagation",
		policy: c.driveDiscoveryPolicy,
		match:  isTransientDrivesDiscoveryError,
	}, func() ([]Drive, error) {
		drives, err := c.drivesList(ctx)
		if err != nil {
			return nil, err
		}

		return c.normalizeDrives(ctx, drives)
	})
}

// drivesList performs a single GET /me/drives call without retry.
func (c *Client) drivesList(ctx context.Context) ([]Drive, error) {
	resp, err := c.do(ctx, http.MethodGet, "/me/drives", nil)
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

// normalizeDrives applies Graph discovery quirks after a successful /me/drives
// response. Personal accounts expose hidden Photos/album system drives through
// /me/drives; they look like normal personal drives but fail when used. The
// only authoritative primary-drive source for Personal accounts is /me/drive,
// so replace every personal entry with that single drive before returning.
func (c *Client) normalizeDrives(ctx context.Context, drives []Drive) ([]Drive, error) {
	if !hasPersonalDrive(drives) {
		return drives, nil
	}

	primary, err := c.PrimaryDrive(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching primary drive: %w", err)
	}

	normalized := replacePersonalDrives(drives, *primary)

	c.logger.Info("normalized personal drive discovery",
		slog.Int("raw_count", len(drives)),
		slog.Int("normalized_count", len(normalized)),
		slog.String("primary_drive_id", primary.ID.String()),
	)

	return normalized, nil
}

func hasPersonalDrive(drives []Drive) bool {
	for _, drive := range drives {
		if drive.DriveType == driveid.DriveTypePersonal {
			return true
		}
	}

	return false
}

func replacePersonalDrives(drives []Drive, primary Drive) []Drive {
	normalized := make([]Drive, 0, len(drives))
	insertedPrimary := false

	for _, drive := range drives {
		if drive.DriveType != driveid.DriveTypePersonal {
			normalized = append(normalized, drive)
			continue
		}

		if insertedPrimary {
			continue
		}

		normalized = append(normalized, primary)
		insertedPrimary = true
	}

	return normalized
}

// PrimaryDrive returns the authenticated user's primary OneDrive via GET /me/drive.
// This is the correct way to discover the user's main drive. Do NOT use Drives()[0]
// because /me/drives returns phantom system drives (Photos face crops, album
// metadata) in non-deterministic order on personal accounts. These system drives
// return HTTP 400 "ObjectHandle is Invalid" when accessed.
func (c *Client) PrimaryDrive(ctx context.Context) (*Drive, error) {
	c.logger.Info("fetching primary drive")

	resp, err := c.do(ctx, http.MethodGet, "/me/drive", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var dr driveResponse
	if err := json.NewDecoder(resp.Body).Decode(&dr); err != nil {
		return nil, fmt.Errorf("graph: decoding primary drive response: %w", err)
	}

	drive := dr.toDrive()

	c.logger.Debug("fetched primary drive",
		slog.String("id", drive.ID.String()),
		slog.String("name", drive.Name),
		slog.String("drive_type", drive.DriveType),
	)

	return &drive, nil
}

// Drive returns a specific drive by ID.
func (c *Client) Drive(ctx context.Context, driveID driveid.ID) (*Drive, error) {
	c.logger.Info("fetching drive",
		slog.String("drive_id", driveID.String()),
	)

	path := fmt.Sprintf("/drives/%s", driveID)

	resp, err := c.do(ctx, http.MethodGet, path, nil)
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

// Organization returns the authenticated user's organization.
// Personal accounts return an empty Organization (the API returns an empty array).
// Business/education accounts return the first organization's display name,
// which is used for sync directory naming (e.g., "~/OneDrive - Contoso").
func (c *Client) Organization(ctx context.Context) (*Organization, error) {
	c.logger.Info("fetching user organization")

	resp, err := c.do(ctx, http.MethodGet, "/me/organization", nil)
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
