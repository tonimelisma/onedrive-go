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

	"github.com/tonimelisma/onedrive-go/internal/driveid"
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

// ErrInvalidPath is returned when a remote path is empty or has a leading slash.
// These indicate a caller bug — path-based API methods require a relative path
// without leading slashes. For root, callers should use the ID-based methods instead.
var ErrInvalidPath = errors.New("graph: invalid remote path (empty or has leading slash)")

// validateRemotePath rejects empty paths and paths with a leading slash.
// Both produce malformed Graph API URLs (e.g. "/drives/d/root:/:" or "/drives/d/root://foo:").
func validateRemotePath(remotePath string) error {
	if remotePath == "" || strings.HasPrefix(remotePath, "/") {
		return ErrInvalidPath
	}

	return nil
}

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
	Root                 *json.RawMessage `json:"root"`
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
		IsRoot:      d.Root != nil,
		IsDeleted:   d.Deleted != nil,
		IsPackage:   d.Package != nil,
		ChildCount:  ChildCountUnknown,
		DownloadURL: d.DownloadURL,
	}

	// Normalize DriveID via driveid.New — lowercase + zero-pad for short IDs.
	// Graph API returns inconsistent casing across endpoints (see docs/tier1-research/).
	//
	// Both DriveID and ParentDriveID come from parentReference.driveId — the
	// Graph API only provides one drive identifier per item. For same-drive
	// items these are always equal. Cross-drive resolution (e.g. shared items)
	// happens at a higher layer (sync's resolveParentDriveID).
	if d.ParentReference != nil {
		item.DriveID = driveid.New(d.ParentReference.DriveID)
		item.ParentID = d.ParentReference.ID
		item.ParentDriveID = driveid.New(d.ParentReference.DriveID)
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

	// Timestamps — validate and fallback to now if invalid.
	// Deleted items routinely have empty timestamps (known OneDrive API behavior),
	// so we log at DEBUG instead of WARN to avoid noise.
	item.CreatedAt = parseTimestamp(d.CreatedDateTime, "createdDateTime", d.ID, item.IsDeleted, logger)
	item.ModifiedAt = parseTimestamp(d.LastModifiedDateTime, "lastModifiedDateTime", d.ID, item.IsDeleted, logger)

	return item
}

// parseTimestamp parses an RFC3339 timestamp and validates the year range.
// Invalid or out-of-range timestamps are replaced with time.Now().UTC() and logged.
// For deleted items, anomalies are logged at DEBUG (expected API behavior);
// for live items, they're logged at WARN (genuinely unexpected).
func parseTimestamp(raw, field, itemID string, isDeleted bool, logger *slog.Logger) time.Time {
	logFunc := logger.Warn
	if isDeleted {
		logFunc = logger.Debug
	}

	if raw == "" {
		logFunc("empty timestamp, using current time",
			slog.String("field", field),
			slog.String("item_id", itemID),
		)

		return time.Now().UTC()
	}

	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		logFunc("invalid timestamp, using current time",
			slog.String("field", field),
			slog.String("item_id", itemID),
			slog.String("raw", raw),
			slog.String("error", err.Error()),
		)

		return time.Now().UTC()
	}

	if t.Year() < minValidYear || t.Year() > maxValidYear {
		logFunc("timestamp out of valid range, using current time",
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
func (c *Client) GetItem(ctx context.Context, driveID driveid.ID, itemID string) (*Item, error) {
	c.logger.Info("getting item",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", itemID),
	)

	return c.fetchItem(ctx, fmt.Sprintf("/drives/%s/items/%s", driveID, itemID))
}

// GetItemByPath retrieves a drive item by its path relative to the drive root.
// The path must NOT have a leading slash and must not be empty — these are caller
// bugs that produce malformed API URLs. Returns ErrInvalidPath for both cases.
// For root, callers should use GetItem with itemID "root" instead.
func (c *Client) GetItemByPath(ctx context.Context, driveID driveid.ID, remotePath string) (*Item, error) {
	if err := validateRemotePath(remotePath); err != nil {
		return nil, err
	}

	c.logger.Info("getting item by path",
		slog.String("drive_id", driveID.String()),
		slog.String("path", remotePath),
	)

	return c.fetchItem(ctx, fmt.Sprintf("/drives/%s/root:/%s:", driveID, encodePathSegments(remotePath)))
}

// ListChildren returns all children of a folder, handling pagination automatically.
func (c *Client) ListChildren(ctx context.Context, driveID driveid.ID, parentID string) ([]Item, error) {
	return c.fetchAllChildren(
		ctx,
		fmt.Sprintf("/drives/%s/items/%s/children?$top=%d", driveID, parentID, listChildrenPageSize),
		"listing children",
		"listed children complete",
		[]slog.Attr{
			slog.String("drive_id", driveID.String()),
			slog.String("parent_id", parentID),
		},
	)
}

// ListChildrenByPath returns all children of a folder identified by path,
// handling pagination automatically. The path must NOT have a leading slash and
// must not be empty — returns ErrInvalidPath for both cases.
// For root, callers should use ListChildren with parentID "root" instead.
func (c *Client) ListChildrenByPath(ctx context.Context, driveID driveid.ID, remotePath string) ([]Item, error) {
	if err := validateRemotePath(remotePath); err != nil {
		return nil, err
	}

	return c.fetchAllChildren(
		ctx,
		fmt.Sprintf("/drives/%s/root:/%s:/children?$top=%d", driveID, encodePathSegments(remotePath), listChildrenPageSize),
		"listing children by path",
		"listed children by path complete",
		[]slog.Attr{
			slog.String("drive_id", driveID.String()),
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

	resp, err := c.Do(ctx, http.MethodPatch, path, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var dir driveItemResponse
	if err := json.NewDecoder(resp.Body).Decode(&dir); err != nil {
		return nil, fmt.Errorf("graph: decoding fileSystemInfo response: %w", err)
	}

	item := dir.toItem(c.logger)

	return &item, nil
}

// DeleteItem deletes a drive item. Returns nil on success (HTTP 204).
func (c *Client) DeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error {
	c.logger.Info("deleting item",
		slog.String("drive_id", driveID.String()),
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
