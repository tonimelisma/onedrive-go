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
	c.logger.Debug("creating folder",
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
	defer resp.Body.Close() //nolint:errcheck // response body is read-only; its close reports nothing a caller can act on

	// Reads one byte past the cap so an oversized body is reported rather than
	// silently truncated into a confusing JSON syntax error.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxItemResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("graph: reading create folder response: %w", err)
	}

	if len(body) > maxItemResponseSize {
		return nil, fmt.Errorf("graph: create folder response exceeds %d bytes", maxItemResponseSize)
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
	c.logger.Debug("create folder returned empty success body, confirming by parent listing",
		slog.String("drive_id", driveID.String()),
		slog.String("parent_id", parentID),
		slog.String("name", name),
	)

	var lastErr error

	for attempt := range c.policies.createFolderReadback.MaxAttempts {
		children, err := c.ListChildren(ctx, driveID, parentID)
		if err == nil {
			if item, ok := findCreatedFolder(children, name); ok {
				return &item, nil
			}

			lastErr = ErrNotFound
		} else {
			lastErr = err
		}

		if attempt >= c.policies.createFolderReadback.MaxAttempts-1 {
			break
		}

		backoff := c.policies.createFolderReadback.Delay(attempt)
		c.logger.Debug("waiting to confirm created folder after empty success body",
			slog.String("drive_id", driveID.String()),
			slog.String("parent_id", parentID),
			slog.String("name", name),
			slog.Int("attempt", attempt+1),
			slog.Int("max_attempts", c.policies.createFolderReadback.MaxAttempts),
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
	return c.MoveItemIfMatch(ctx, driveID, itemID, newParentID, newName, "")
}

// MoveItemIfMatch moves and/or renames an item, adding If-Match when ifMatch
// is non-empty so Graph rejects the mutation if the item changed after the
// caller's live preflight.
func (c *Client) MoveItemIfMatch(
	ctx context.Context,
	driveID driveid.ID,
	itemID string,
	newParentID string,
	newName string,
	ifMatch string,
) (*Item, error) {
	if newParentID == "" && newName == "" {
		return nil, ErrMoveNoChanges
	}

	c.logger.Debug("moving item",
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

	resp, err := c.doRequest(ctx, http.MethodPatch, path, bytes.NewReader(bodyBytes), ifMatchHeaders(ifMatch))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // response body is read-only; its close reports nothing a caller can act on

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
		return nil, relabelTimestampRangeError(err, mtime)
	}
	defer resp.Body.Close() //nolint:errcheck // response body is read-only; its close reports nothing a caller can act on

	return c.decodeMutatedItem(ctx, resp.Body, "fileSystemInfo", func(ctx context.Context) (*Item, error) {
		return c.GetItem(ctx, driveID, itemID)
	})
}

// ErrUploadCompletedWithoutItem reports a 2xx upload completion whose body
// did not carry the item: empty (204, or a proxy-trimmed 200), not JSON, a
// 201 carrying {"error":...}, or an item without an id. The content may well
// have landed, so callers look the item up instead of failing the upload or
// recording an item without identity: such an item was written to the
// baseline with an empty primary key, and a second one rewrote the first
// row's path (D-035).
var ErrUploadCompletedWithoutItem = errors.New("graph: upload completed without returning the item")

// errMutationReturnedNoItem is the readback-less form of the same condition.
var errMutationReturnedNoItem = errors.New("graph: mutation succeeded without returning the item")

// decodeItemBody reads a success-path driveItem body. hasItem is false when
// the body is empty, is not a driveItem, or carries no id; err is only a read
// failure or an oversized body.
func (c *Client) decodeItemBody(body io.Reader, desc string) (item *Item, hasItem bool, err error) {
	// Reads one byte past the cap so an oversized body is reported rather
	// than silently truncated.
	raw, err := io.ReadAll(io.LimitReader(body, maxItemResponseSize+1))
	if err != nil {
		return nil, false, fmt.Errorf("graph: reading %s response: %w", desc, err)
	}

	if len(raw) > maxItemResponseSize {
		return nil, false, fmt.Errorf("graph: %s response exceeds %d bytes", desc, maxItemResponseSize)
	}

	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, false, nil
	}

	// A body that is not a driveItem is not a failure of the mutation: the
	// caller resolves the item another way, so the decode error is recorded
	// here and goes no further.
	var dir driveItemResponse
	jsonErr := json.Unmarshal(raw, &dir)
	if jsonErr != nil || dir.ID == "" {
		reason := "no id"
		if jsonErr != nil {
			reason = jsonErr.Error()
		}

		c.logger.Debug("success response carried no item",
			slog.String("operation", desc),
			slog.Int("body_bytes", len(raw)),
			slog.String("reason", reason),
		)

		return nil, false, nil
	}

	decoded := dir.toItem(c.logger)
	normalizeSingleItem(&decoded, c.logger)

	return &decoded, true, nil
}

// decodeMutatedItem decodes the item a 2xx mutation returned, reading it back
// by ID when the body did not carry it. A 204 or an empty 200 used to fail as
// a JSON "EOF" decode error, which classified the mutation that had just
// succeeded as retry work (D-047).
func (c *Client) decodeMutatedItem(
	ctx context.Context, body io.Reader, desc string, readback func(context.Context) (*Item, error),
) (*Item, error) {
	item, hasItem, err := c.decodeItemBody(body, desc)
	if err != nil {
		return nil, err
	}

	if hasItem {
		return item, nil
	}

	if readback == nil {
		return nil, fmt.Errorf("%s: %w", desc, errMutationReturnedNoItem)
	}

	item, err = readback(ctx)
	if err != nil {
		return nil, fmt.Errorf("graph: reading back item after %s: %w", desc, err)
	}

	return item, nil
}

// PermanentDeleteItem permanently deletes a drive item (bypasses the recycle bin).
// Works on Personal and Business/SharePoint drives (Personal returned 204 in a
// 2026-10-25 live probe; earlier docs said it returned 405).
// Uses POST /drives/{driveId}/items/{itemId}/permanentDelete.
func (c *Client) PermanentDeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error {
	c.logger.Debug("permanently deleting item",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", itemID),
	)

	return c.deleteAndDrain(ctx, http.MethodPost,
		fmt.Sprintf("/drives/%s/items/%s/permanentDelete", driveID, itemID))
}

// DeleteItem deletes a drive item. Returns nil on success (HTTP 204).
func (c *Client) DeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error {
	return c.DeleteItemIfMatch(ctx, driveID, itemID, "")
}

// DeleteItemIfMatch deletes a drive item, adding If-Match when ifMatch is
// non-empty so Graph rejects the mutation if the item changed after the
// caller's live preflight.
func (c *Client) DeleteItemIfMatch(ctx context.Context, driveID driveid.ID, itemID string, ifMatch string) error {
	c.logger.Debug("deleting item",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", itemID),
	)

	return c.deleteAndDrainWithHeaders(ctx, http.MethodDelete,
		fmt.Sprintf("/drives/%s/items/%s", driveID, itemID), ifMatchHeaders(ifMatch))
}

// deleteAndDrain sends a request and drains the response body to reuse the connection.
func (c *Client) deleteAndDrain(ctx context.Context, method, path string) error {
	return c.deleteAndDrainWithHeaders(ctx, method, path, nil)
}

func (c *Client) deleteAndDrainWithHeaders(ctx context.Context, method, path string, headers http.Header) error {
	resp, err := c.doRequest(ctx, method, path, nil, headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // response body is read-only; its close reports nothing a caller can act on

	if _, copyErr := io.Copy(io.Discard, resp.Body); copyErr != nil {
		return fmt.Errorf("graph: draining response body: %w", copyErr)
	}

	return nil
}

func ifMatchHeaders(ifMatch string) http.Header {
	if ifMatch == "" {
		return nil
	}

	headers := make(http.Header)
	headers.Set("If-Match", ifMatch)
	return headers
}
