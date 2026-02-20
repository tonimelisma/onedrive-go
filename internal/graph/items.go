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
	"net/url"
	"strings"
	"time"
)

// listChildrenPageSize is the $top value for ListChildren requests.
// 200 is the maximum allowed by the Graph API for drive item collections.
const listChildrenPageSize = 200

// Timestamp validation bounds — timestamps outside this range are replaced
// with the current time and a warning is logged.
const (
	minValidYear = 1970
	maxValidYear = 2100
)

// encodePathSegments URL-encodes each segment of a slash-separated path.
// Characters like #, ?, %, and spaces are encoded per-segment so the
// resulting path is safe for interpolation into Graph API URLs.
func encodePathSegments(path string) string {
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}

	return strings.Join(segments, "/")
}

// driveItemResponse mirrors the Graph API driveItem JSON exactly.
// Unexported — callers use Item via toItem() normalization.
type driveItemResponse struct {
	ID                   string           `json:"id"`
	Name                 string           `json:"name"`
	Size                 int64            `json:"size"`
	ETag                 string           `json:"eTag"`
	CTag                 string           `json:"cTag"`
	CreatedDateTime      string           `json:"createdDateTime"`
	LastModifiedDateTime string           `json:"lastModifiedDateTime"`
	ParentReference      *parentRef       `json:"parentReference"`
	File                 *fileFacet       `json:"file"`
	Folder               *folderFacet     `json:"folder"`
	Deleted              *json.RawMessage `json:"deleted"`
	Package              *json.RawMessage `json:"package"`
	DownloadURL          string           `json:"@microsoft.graph.downloadUrl"` //nolint:tagliatelle // Graph API annotation key
}

type parentRef struct {
	ID      string `json:"id"`
	DriveID string `json:"driveId"`
}

type fileFacet struct {
	MimeType string     `json:"mimeType"`
	Hashes   *hashFacet `json:"hashes"`
}

type hashFacet struct {
	QuickXorHash string `json:"quickXorHash"`
	SHA1Hash     string `json:"sha1Hash"`
	SHA256Hash   string `json:"sha256Hash"`
}

type folderFacet struct {
	ChildCount int `json:"childCount"`
}

type listChildrenResponse struct {
	Value    []driveItemResponse `json:"value"`
	NextLink string              `json:"@odata.nextLink"` //nolint:tagliatelle // OData annotation key
}

type createFolderRequest struct {
	Name             string      `json:"name"`
	Folder           folderFacet `json:"folder"`
	ConflictBehavior string      `json:"@microsoft.graph.conflictBehavior"` //nolint:tagliatelle // Graph API annotation key
}

type moveItemRequest struct {
	ParentReference *moveParentRef `json:"parentReference,omitempty"`
	Name            string         `json:"name,omitempty"`
}

type moveParentRef struct {
	ID string `json:"id"`
}

// toItem normalizes a Graph API driveItem response into our Item type.
func (d *driveItemResponse) toItem(logger *slog.Logger) Item {
	item := Item{
		ID:          d.ID,
		Name:        d.Name,
		Size:        d.Size,
		ETag:        d.ETag,
		CTag:        d.CTag,
		IsFolder:    d.Folder != nil,
		IsDeleted:   d.Deleted != nil,
		IsPackage:   d.Package != nil,
		ChildCount:  ChildCountUnknown,
		DownloadURL: d.DownloadURL,
	}

	// Normalize DriveID to lowercase — Graph API returns inconsistent casing
	// for drive IDs across endpoints (see docs/tier1-research/).
	if d.ParentReference != nil {
		item.DriveID = strings.ToLower(d.ParentReference.DriveID)
		item.ParentID = d.ParentReference.ID
		item.ParentDriveID = strings.ToLower(d.ParentReference.DriveID)
	}

	// Folder child count
	if d.Folder != nil {
		item.ChildCount = d.Folder.ChildCount
	}

	// File hashes — nil-safe at each level
	if d.File != nil {
		item.MimeType = d.File.MimeType

		if d.File.Hashes != nil {
			item.QuickXorHash = d.File.Hashes.QuickXorHash
			item.SHA1Hash = d.File.Hashes.SHA1Hash
			item.SHA256Hash = d.File.Hashes.SHA256Hash
		}
	}

	// Timestamps — validate and fallback to now if invalid
	item.CreatedAt = parseTimestamp(d.CreatedDateTime, "createdDateTime", d.ID, logger)
	item.ModifiedAt = parseTimestamp(d.LastModifiedDateTime, "lastModifiedDateTime", d.ID, logger)

	return item
}

// parseTimestamp parses an RFC3339 timestamp and validates the year range.
// Invalid or out-of-range timestamps are replaced with time.Now().UTC() and logged.
func parseTimestamp(raw, field, itemID string, logger *slog.Logger) time.Time {
	if raw == "" {
		logger.Warn("empty timestamp, using current time",
			slog.String("field", field),
			slog.String("item_id", itemID),
		)

		return time.Now().UTC()
	}

	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		logger.Warn("invalid timestamp, using current time",
			slog.String("field", field),
			slog.String("item_id", itemID),
			slog.String("raw", raw),
			slog.String("error", err.Error()),
		)

		return time.Now().UTC()
	}

	if t.Year() < minValidYear || t.Year() > maxValidYear {
		logger.Warn("timestamp out of valid range, using current time",
			slog.String("field", field),
			slog.String("item_id", itemID),
			slog.String("raw", raw),
		)

		return time.Now().UTC()
	}

	return t
}

// fetchItem fetches a single drive item from the given API path and decodes it.
// Shared by GetItem (ID-based) and GetItemByPath (path-based) to avoid duplication.
func (c *Client) fetchItem(ctx context.Context, apiPath string) (*Item, error) {
	resp, err := c.Do(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var dir driveItemResponse
	if err := json.NewDecoder(resp.Body).Decode(&dir); err != nil {
		return nil, fmt.Errorf("graph: decoding item response: %w", err)
	}

	item := dir.toItem(c.logger)

	return &item, nil
}

// fetchAllChildren paginates through all children starting from the given API path,
// logging entry/completion with the provided attrs. Shared by ListChildren (ID-based)
// and ListChildrenByPath (path-based) to avoid duplication.
func (c *Client) fetchAllChildren(
	ctx context.Context,
	apiPath string,
	entryMsg string,
	doneMsg string,
	logAttrs []slog.Attr,
) ([]Item, error) {
	args := make([]any, 0, len(logAttrs))
	for _, a := range logAttrs {
		args = append(args, a)
	}

	c.logger.Info(entryMsg, args...)

	var items []Item

	page := 1

	for apiPath != "" {
		pageItems, nextPath, err := c.listChildrenPage(ctx, apiPath, page)
		if err != nil {
			return nil, err
		}

		items = append(items, pageItems...)
		apiPath = nextPath
		page++
	}

	args = append(args, slog.Int("total_items", len(items)))
	c.logger.Info(doneMsg, args...)

	return items, nil
}

// GetItem retrieves a single drive item by ID.
func (c *Client) GetItem(ctx context.Context, driveID, itemID string) (*Item, error) {
	c.logger.Info("getting item",
		slog.String("drive_id", driveID),
		slog.String("item_id", itemID),
	)

	return c.fetchItem(ctx, fmt.Sprintf("/drives/%s/items/%s", driveID, itemID))
}

// GetItemByPath retrieves a drive item by its path relative to the drive root.
// The path must NOT have a leading slash (caller strips it).
// For root, callers should use GetItem with itemID "root" instead.
func (c *Client) GetItemByPath(ctx context.Context, driveID, remotePath string) (*Item, error) {
	c.logger.Info("getting item by path",
		slog.String("drive_id", driveID),
		slog.String("path", remotePath),
	)

	return c.fetchItem(ctx, fmt.Sprintf("/drives/%s/root:/%s:", driveID, encodePathSegments(remotePath)))
}

// ListChildren returns all children of a folder, handling pagination automatically.
func (c *Client) ListChildren(ctx context.Context, driveID, parentID string) ([]Item, error) {
	return c.fetchAllChildren(
		ctx,
		fmt.Sprintf("/drives/%s/items/%s/children?$top=%d", driveID, parentID, listChildrenPageSize),
		"listing children",
		"listed children complete",
		[]slog.Attr{
			slog.String("drive_id", driveID),
			slog.String("parent_id", parentID),
		},
	)
}

// ListChildrenByPath returns all children of a folder identified by path,
// handling pagination automatically. The path must NOT have a leading slash.
// For root, callers should use ListChildren with parentID "root" instead.
func (c *Client) ListChildrenByPath(ctx context.Context, driveID, remotePath string) ([]Item, error) {
	return c.fetchAllChildren(
		ctx,
		fmt.Sprintf("/drives/%s/root:/%s:/children?$top=%d", driveID, encodePathSegments(remotePath), listChildrenPageSize),
		"listing children by path",
		"listed children by path complete",
		[]slog.Attr{
			slog.String("drive_id", driveID),
			slog.String("remote_path", remotePath),
		},
	)
}

// listChildrenPage fetches a single page of children and returns the items
// and the next page path (empty if no more pages).
func (c *Client) listChildrenPage(ctx context.Context, path string, page int) ([]Item, string, error) {
	resp, err := c.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	var lcr listChildrenResponse
	if err := json.NewDecoder(resp.Body).Decode(&lcr); err != nil {
		return nil, "", fmt.Errorf("graph: decoding children response: %w", err)
	}

	items := make([]Item, 0, len(lcr.Value))
	for i := range lcr.Value {
		items = append(items, lcr.Value[i].toItem(c.logger))
	}

	c.logger.Debug("fetched children page",
		slog.Int("page", page),
		slog.Int("count", len(items)),
	)

	var nextPath string
	if lcr.NextLink != "" {
		var stripErr error

		nextPath, stripErr = c.stripBaseURL(lcr.NextLink)
		if stripErr != nil {
			return nil, "", stripErr
		}
	}

	return items, nextPath, nil
}

// stripBaseURL removes the client's base URL prefix from a full URL,
// returning the path + query string for use with Do().
// Returns an error if the URL doesn't start with the expected base.
func (c *Client) stripBaseURL(fullURL string) (string, error) {
	if !strings.HasPrefix(fullURL, c.baseURL) {
		return "", fmt.Errorf("graph: nextLink URL %q does not match base URL %q", fullURL, c.baseURL)
	}

	return fullURL[len(c.baseURL):], nil
}

// CreateFolder creates a new folder under the given parent.
// Uses conflictBehavior "fail" — returns ErrConflict (409) on name collision.
func (c *Client) CreateFolder(ctx context.Context, driveID, parentID, name string) (*Item, error) {
	c.logger.Info("creating folder",
		slog.String("drive_id", driveID),
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

	resp, err := c.Do(ctx, http.MethodPost, path, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var dir driveItemResponse
	if err := json.NewDecoder(resp.Body).Decode(&dir); err != nil {
		return nil, fmt.Errorf("graph: decoding create folder response: %w", err)
	}

	item := dir.toItem(c.logger)

	return &item, nil
}

// ErrMoveNoChanges is returned when MoveItem is called with both newParentID
// and newName empty — at least one must be specified.
var ErrMoveNoChanges = errors.New("graph: MoveItem requires at least one of newParentID or newName")

// MoveItem moves and/or renames an item. At least one of newParentID or newName must be non-empty.
func (c *Client) MoveItem(ctx context.Context, driveID, itemID, newParentID, newName string) (*Item, error) {
	if newParentID == "" && newName == "" {
		return nil, ErrMoveNoChanges
	}

	c.logger.Info("moving item",
		slog.String("drive_id", driveID),
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

	resp, err := c.Do(ctx, http.MethodPatch, path, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var dir driveItemResponse
	if err := json.NewDecoder(resp.Body).Decode(&dir); err != nil {
		return nil, fmt.Errorf("graph: decoding move response: %w", err)
	}

	item := dir.toItem(c.logger)

	return &item, nil
}

// DeleteItem deletes a drive item. Returns nil on success (HTTP 204).
func (c *Client) DeleteItem(ctx context.Context, driveID, itemID string) error {
	c.logger.Info("deleting item",
		slog.String("drive_id", driveID),
		slog.String("item_id", itemID),
	)

	path := fmt.Sprintf("/drives/%s/items/%s", driveID, itemID)

	resp, err := c.Do(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}

	// 204 No Content — drain and close to reuse connection.
	defer resp.Body.Close()

	if _, copyErr := io.Copy(io.Discard, resp.Body); copyErr != nil {
		return fmt.Errorf("graph: draining delete response body: %w", copyErr)
	}

	return nil
}
