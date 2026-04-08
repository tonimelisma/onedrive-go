package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// CreateFolder creates a new folder under the given parent.
// Uses conflictBehavior "fail" — returns ErrConflict (409) on name collision.
func (c *Client) CreateFolder(ctx context.Context, driveID driveid.ID, parentID, name string) (*Item, error) {
	c.logger.Info("creating folder",
		slog.String("drive_id", driveID.String()),
		slog.String("parent_id", parentID),
		slog.String("name", name),
	)

	path := fmt.Sprintf("/drives/%s/items/%s/children", driveID, parentID)

	reqBody := createFolderRequest{
		Name:             name,
		Folder:           folderFacet{},
		ConflictBehavior: "fail",
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("graph: marshaling create folder request: %w", err)
	}

	resp, err := c.do(ctx, http.MethodPost, path, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("graph: reading create folder response: %w", err)
	}

	if len(bytes.TrimSpace(body)) == 0 {
		return c.readCreatedFolderAfterEmptySuccessBody(ctx, driveID, parentID, name)
	}

	var dir driveItemResponse
	if err := json.Unmarshal(body, &dir); err != nil {
		return nil, fmt.Errorf("graph: decoding create folder response: %w", err)
	}

	item := dir.toItem(c.logger)
	normalizeSingleItem(&item, c.logger)

	return &item, nil
}

func (c *Client) readCreatedFolderAfterEmptySuccessBody(
	ctx context.Context,
	driveID driveid.ID,
	parentID string,
	name string,
) (*Item, error) {
	c.logger.Warn("create folder returned empty success body, confirming by parent listing",
		slog.String("drive_id", driveID.String()),
		slog.String("parent_id", parentID),
		slog.String("name", name),
	)

	var lastErr error

	for attempt := range c.createFolderReadbackPolicy.MaxAttempts {
		children, err := c.ListChildren(ctx, driveID, parentID)
		if err == nil {
			if item, ok := findCreatedFolder(children, name); ok {
				return &item, nil
			}

			lastErr = ErrNotFound
		} else {
			lastErr = err
		}

		if attempt >= c.createFolderReadbackPolicy.MaxAttempts-1 {
			break
		}

		backoff := c.createFolderReadbackPolicy.Delay(attempt)
		c.logger.Debug("waiting to confirm created folder after empty success body",
			slog.String("drive_id", driveID.String()),
			slog.String("parent_id", parentID),
			slog.String("name", name),
			slog.Int("attempt", attempt+1),
			slog.Int("max_attempts", c.createFolderReadbackPolicy.MaxAttempts),
			slog.Duration("backoff", backoff),
		)

		if sleepErr := retry.TimeSleep(ctx, backoff); sleepErr != nil {
			return nil, fmt.Errorf("graph: create folder read-back canceled: %w", sleepErr)
		}
	}

	if lastErr == nil {
		lastErr = ErrNotFound
	}

	return nil, fmt.Errorf("graph: create folder empty success response: %w", lastErr)
}

func findCreatedFolder(children []Item, name string) (Item, bool) {
	var foldedMatch Item
	hasFoldedMatch := false

	for i := range children {
		child := children[i]
		if !child.IsFolder {
			continue
		}

		if child.Name == name {
			return child, true
		}

		if strings.EqualFold(child.Name, name) {
			if hasFoldedMatch {
				return Item{}, false
			}

			foldedMatch = child
			hasFoldedMatch = true
		}
	}

	return foldedMatch, hasFoldedMatch
}

// ErrMoveNoChanges is returned when MoveItem is called with both newParentID
// and newName empty — at least one must be specified.
var ErrMoveNoChanges = errors.New("graph: MoveItem requires at least one of newParentID or newName")

// MoveItem moves and/or renames an item. At least one of newParentID or newName must be non-empty.
func (c *Client) MoveItem(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*Item, error) {
	if newParentID == "" && newName == "" {
		return nil, ErrMoveNoChanges
	}

	c.logger.Info("moving item",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", itemID),
		slog.String("new_parent_id", newParentID),
		slog.String("new_name", newName),
	)

	path := fmt.Sprintf("/drives/%s/items/%s", driveID, itemID)

	req := moveItemRequest{}
	if newParentID != "" {
		req.ParentReference = &moveParentRef{ID: newParentID}
	}

	if newName != "" {
		req.Name = newName
	}

	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("graph: marshaling move request: %w", err)
	}

	resp, err := c.do(ctx, http.MethodPatch, path, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var dir driveItemResponse
	if err := json.NewDecoder(resp.Body).Decode(&dir); err != nil {
		return nil, fmt.Errorf("graph: decoding move response: %w", err)
	}

	item := dir.toItem(c.logger)
	normalizeSingleItem(&item, c.logger)

	return &item, nil
}

// updateFileSystemInfoRequest is the JSON body for PATCH requests that set
// fileSystemInfo timestamps. Reuses the fileSystemInfo type from upload.go.
type updateFileSystemInfoRequest struct {
	FileSystemInfo *fileSystemInfo `json:"fileSystemInfo"`
}

// UpdateFileSystemInfo sets the lastModifiedDateTime on a remote item via PATCH.
// Used after simple upload (which cannot include metadata in the PUT body) to
// preserve local mtime on the server. Returns the patched item.
func (c *Client) UpdateFileSystemInfo(
	ctx context.Context, driveID driveid.ID, itemID string, mtime time.Time,
) (*Item, error) {
	c.logger.Debug("updating fileSystemInfo",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", itemID),
		slog.Time("mtime", mtime),
	)

	path := fmt.Sprintf("/drives/%s/items/%s", driveID, itemID)

	reqBody := updateFileSystemInfoRequest{
		FileSystemInfo: &fileSystemInfo{
			LastModifiedDateTime: mtime.UTC().Format(time.RFC3339),
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("graph: marshaling fileSystemInfo request: %w", err)
	}

	resp, err := c.do(ctx, http.MethodPatch, path, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var dir driveItemResponse
	if err := json.NewDecoder(resp.Body).Decode(&dir); err != nil {
		return nil, fmt.Errorf("graph: decoding fileSystemInfo response: %w", err)
	}

	item := dir.toItem(c.logger)
	normalizeSingleItem(&item, c.logger)

	return &item, nil
}

// PermanentDeleteItem permanently deletes a drive item (bypasses the recycle bin).
// Only supported on Business/SharePoint accounts — Personal accounts return 405.
// Uses POST /drives/{driveId}/items/{itemId}/permanentDelete.
func (c *Client) PermanentDeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error {
	c.logger.Info("permanently deleting item",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", itemID),
	)

	return c.deleteAndDrain(ctx, http.MethodPost,
		fmt.Sprintf("/drives/%s/items/%s/permanentDelete", driveID, itemID))
}

// DeleteItem deletes a drive item. Returns nil on success (HTTP 204).
func (c *Client) DeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error {
	c.logger.Info("deleting item",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", itemID),
	)

	return c.deleteAndDrain(ctx, http.MethodDelete,
		fmt.Sprintf("/drives/%s/items/%s", driveID, itemID))
}

// deleteAndDrain sends a request and drains the response body to reuse the connection.
func (c *Client) deleteAndDrain(ctx context.Context, method, path string) error {
	resp, err := c.do(ctx, method, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if _, copyErr := io.Copy(io.Discard, resp.Body); copyErr != nil {
		return fmt.Errorf("graph: draining response body: %w", copyErr)
	}

	return nil
}
