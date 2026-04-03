package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Permission represents a single permission entry on a drive item.
// Used to determine whether the current user has write access to shared folders.
type Permission struct {
	ID    string   `json:"id"`
	Roles []string `json:"roles"`
}

type listPermissionsResponse struct {
	Value []Permission `json:"value"`
}

// HasWriteAccess returns true if any permission in the slice grants write or
// owner access. Returns false for empty slices (no permissions = no access).
func HasWriteAccess(perms []Permission) bool {
	for i := range perms {
		for _, role := range perms[i].Roles {
			if role == "write" || role == "owner" {
				return true
			}
		}
	}

	return false
}

// ListItemPermissions returns the permissions for a drive item. For non-owner
// callers, only THEIR permissions are returned — ideal for checking our own
// access level on shared folders.
func (c *Client) ListItemPermissions(ctx context.Context, driveID driveid.ID, itemID string) ([]Permission, error) {
	c.logger.Debug("listing item permissions",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", itemID),
	)

	apiPath := fmt.Sprintf("/drives/%s/items/%s/permissions", driveID, itemID)

	resp, err := c.do(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var lpr listPermissionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&lpr); err != nil {
		return nil, fmt.Errorf("graph: decoding permissions response: %w", err)
	}

	return lpr.Value, nil
}

// ListRecycleBinItems returns all items in the drive's recycle bin.
func (c *Client) ListRecycleBinItems(
	ctx context.Context, driveID driveid.ID,
) ([]Item, error) {
	return c.fetchAllChildren(
		ctx,
		fmt.Sprintf("/drives/%s/special/recyclebin/children?$top=%d",
			driveID, listChildrenPageSize),
		"listing recycle bin items",
		"listed recycle bin items complete",
		[]slog.Attr{
			slog.String("drive_id", driveID.String()),
		},
	)
}

// RestoreItem restores a deleted item from the recycle bin to its original
// location. Returns the restored item. Returns ErrConflict if an item with
// the same name already exists at the original location.
func (c *Client) RestoreItem(
	ctx context.Context, driveID driveid.ID, itemID string,
) (*Item, error) {
	c.logger.Info("restoring item",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", itemID),
	)

	apiPath := fmt.Sprintf("/drives/%s/items/%s/restore", driveID, itemID)

	resp, err := c.do(ctx, http.MethodPost, apiPath, http.NoBody)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var dir driveItemResponse
	if err := json.NewDecoder(resp.Body).Decode(&dir); err != nil {
		return nil, fmt.Errorf("graph: decoding restore response: %w", err)
	}

	item := dir.toItem(c.logger)

	return &item, nil
}
