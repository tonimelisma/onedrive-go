package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
)

// fetchItem fetches a single drive item from the given API path and decodes it.
// Shared by GetItem (ID-based) and GetItemByPath (path-based) to avoid duplication.
func (c *Client) fetchItem(ctx context.Context, apiPath string) (*Item, error) {
	resp, err := c.do(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // response body is read-only; its close reports nothing a caller can act on

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

	c.logger.Debug(entryMsg, args...)

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

		if page > defaultMaxListPages {
			return nil, fmt.Errorf("graph: %s exceeded %d pages", entryMsg, defaultMaxListPages)
		}
	}

	args = append(args, slog.Int("total_items", len(items)))
	c.logger.Debug(doneMsg, args...)

	return items, nil
}

// GetItem retrieves a single drive item by ID.
func (c *Client) GetItem(ctx context.Context, driveID driveid.ID, itemID string) (*Item, error) {
	c.logger.Debug("getting item",
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

	c.logger.Debug("getting item by path",
		slog.String("drive_id", driveID.String()),
		slog.String("path", remotePath),
	)

	item, err := c.fetchItemByPathChunks(ctx, driveID, remotePath)
	if err != nil {
		return nil, err
	}

	if exactPath, ok := itemExactRootRelativePath(item); ok {
		if namekey.EqualPath(exactPath, remotePath) {
			return item, nil
		}

		return nil, fmt.Errorf("graph: requested path %q resolved to %q: %w", remotePath, exactPath, ErrNotFound)
	}

	c.logger.Debug("path lookup missing exact parent path, using leaf fallback",
		slog.String("requested_path", remotePath),
		slog.String("resolved_leaf", item.Name),
	)

	if !namekey.Equal(item.Name, pathLeaf(remotePath)) {
		return nil, fmt.Errorf("graph: requested path %q resolved to %q: %w", remotePath, itemBestEffortRootRelativePath(item), ErrNotFound)
	}

	return item, nil
}

// driveRootAlias is the item ID Graph takes for a drive's root folder. It
// names no item: an answer carries the root's own ID as its parent.
const driveRootAlias = "root"

// ErrNameNotPathAddressable reports a name a path-addressed request cannot
// carry: one whose escaped form is longer than maxPathAddressedLength, or one
// holding ':', which ends the path in a path route and which url.PathEscape
// leaves as it is. A caller that must still find such a name lists its
// folder.
var ErrNameNotPathAddressable = errors.New("graph: name cannot be carried in a path-addressed request")

// GetChildByName reads the item OneDrive holds under name directly in the
// folder parentID, with one request (items/{parentID}:/{name}:) instead of a
// listing of the folder. OneDrive resolves the name under its own name
// equivalence, so the answer may carry another spelling of it (letter case,
// Unicode form), and a path query can resolve elsewhere: the answer is the
// item only when it sits directly in parentID and its name equals name by
// namekey.Equal, and is ErrNotFound otherwise. A name the route cannot carry
// is ErrNameNotPathAddressable and an empty name is ErrInvalidPath, both
// before any request.
func (c *Client) GetChildByName(ctx context.Context, driveID driveid.ID, parentID, name string) (*Item, error) {
	if name == "" {
		return nil, ErrInvalidPath
	}
	encoded := url.PathEscape(name)
	if len(encoded) > maxPathAddressedLength || strings.Contains(name, ":") {
		return nil, fmt.Errorf("graph: reading %q: %w", name, ErrNameNotPathAddressable)
	}

	c.logger.Debug("getting child by name",
		slog.String("drive_id", driveID.String()),
		slog.String("parent_id", parentID),
		slog.String("name", name),
	)

	item, err := c.fetchItem(ctx, childPathRoute(driveID, parentID, encoded))
	if err != nil {
		return nil, err
	}
	if !itemSitsIn(item, driveID, parentID) || !namekey.Equal(item.Name, name) {
		return nil, fmt.Errorf("graph: %q in folder %s resolved to %q in folder %s: %w",
			name, parentID, item.Name, item.ParentID, ErrNotFound)
	}

	return item, nil
}

// itemSitsIn reports whether item's parent is the folder parentID of driveID.
// Item IDs carry a drive ID prefix, which OneDrive answers in either letter
// case, so the parent ID is compared without case. For the drive root's
// alias, which no answer carries, the parent path says so: the top of the
// drive, of this drive when the answer names one.
func itemSitsIn(item *Item, driveID driveid.ID, parentID string) bool {
	if strings.EqualFold(item.ParentID, parentID) {
		return true
	}

	return parentID == driveRootAlias && item.ParentPathKnown && item.ParentPath == "" &&
		(item.ParentDriveID.IsZero() || item.ParentDriveID.Equal(driveID))
}

// childPathRoute is the route of the item at encodedPath, a path of escaped
// segments, below the item parentID.
func childPathRoute(driveID driveid.ID, parentID, encodedPath string) string {
	return fmt.Sprintf("/drives/%s/items/%s:/%s:", driveID, url.PathEscape(parentID), encodedPath)
}

// maxPathAddressedLength bounds the encoded path one path-addressed request
// carries. Graph rejects longer URLs with 400 "The length of the URL for this
// request exceeds the configured maxUrlLength value" (measured on a Personal
// drive: a 300-character path resolves, 346 does not), well below OneDrive's
// 400-character path limit, so deep folders could be created but never looked
// up again.
const maxPathAddressedLength = 240

// fetchItemByPathChunks resolves remotePath with as few path-addressed
// requests as fit under maxPathAddressedLength: the first chunk from the
// drive root, each further chunk relative to the item before it.
func (c *Client) fetchItemByPathChunks(ctx context.Context, driveID driveid.ID, remotePath string) (*Item, error) {
	chunks := pathAddressChunks(remotePath, maxPathAddressedLength)

	item, err := c.fetchItem(ctx, fmt.Sprintf("/drives/%s/root:/%s:", driveID, chunks[0]))
	if err != nil {
		return nil, err
	}
	for _, chunk := range chunks[1:] {
		item, err = c.fetchItem(ctx, childPathRoute(driveID, item.ID, chunk))
		if err != nil {
			return nil, err
		}
	}

	return item, nil
}

// pathAddressChunks splits remotePath into encoded chunks of whole segments,
// each at most limit long (a single longer segment stands alone).
func pathAddressChunks(remotePath string, limit int) []string {
	var chunks []string
	current := ""
	for _, segment := range strings.Split(remotePath, "/") {
		encoded := url.PathEscape(segment)
		switch {
		case current == "":
			current = encoded
		case len(current)+1+len(encoded) <= limit:
			current += "/" + encoded
		default:
			chunks = append(chunks, current)
			current = encoded
		}
	}

	return append(chunks, current)
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

	route := fmt.Sprintf("/drives/%s/root:/%s:/children?$top=%d", driveID, encodePathSegments(remotePath), listChildrenPageSize)
	if len(encodePathSegments(remotePath)) > maxPathAddressedLength {
		folder, err := c.fetchItemByPathChunks(ctx, driveID, remotePath)
		if err != nil {
			return nil, err
		}
		route = fmt.Sprintf("/drives/%s/items/%s/children?$top=%d", driveID, folder.ID, listChildrenPageSize)
	}

	return c.fetchAllChildren(
		ctx,
		route,
		"listing children by path",
		"listed children by path complete",
		[]slog.Attr{
			slog.String("drive_id", driveID.String()),
			slog.String("remote_path", remotePath),
		},
	)
}

// ListChildrenRecursive returns all descendants of a folder by recursively
// listing children of subfolders. Returns a flat list including both files
// and folders. Used for enumerating shared folder content on Business/SharePoint
// drives where folder-scoped delta is not available.
func (c *Client) ListChildrenRecursive(ctx context.Context, driveID driveid.ID, folderID string) ([]Item, error) {
	c.logger.Debug("listing children recursively",
		slog.String("drive_id", driveID.String()),
		slog.String("folder_id", folderID),
	)

	items, err := c.listChildrenRecursiveDepth(ctx, driveID, folderID, 0)
	if err != nil {
		return nil, err
	}

	c.logger.Debug("listed children recursively",
		slog.String("drive_id", driveID.String()),
		slog.String("folder_id", folderID),
		slog.Int("total_items", len(items)),
	)

	return items, nil
}

// listChildrenRecursiveDepth is the depth-tracked implementation of ListChildrenRecursive.
func (c *Client) listChildrenRecursiveDepth(ctx context.Context, driveID driveid.ID, folderID string, depth int) ([]Item, error) {
	if depth >= c.maxRecursionDepth {
		return nil, fmt.Errorf("graph: recursive listing exceeded max depth %d at folder %s", c.maxRecursionDepth, folderID)
	}

	children, err := c.ListChildren(ctx, driveID, folderID)
	if err != nil {
		return nil, err
	}

	var allItems []Item

	for i := range children {
		allItems = append(allItems, children[i])

		if children[i].IsFolder {
			descendants, err := c.listChildrenRecursiveDepth(ctx, driveID, children[i].ID, depth+1)
			if err != nil {
				return nil, err
			}

			allItems = append(allItems, descendants...)
		}
	}

	return allItems, nil
}

// listChildrenPage fetches a single page of children and returns the items
// and the next page path (empty if no more pages).
func (c *Client) listChildrenPage(ctx context.Context, path string, page int) ([]Item, string, error) {
	type childrenPage struct {
		items    []Item
		nextPath string
	}

	op := func() (childrenPage, error) {
		resp, err := c.doGetWithHeaders(ctx, path, c.childrenPreferHeader)
		if err != nil {
			return childrenPage{}, err
		}
		defer resp.Body.Close() //nolint:errcheck // response body is read-only; its close reports nothing a caller can act on

		var lcr listChildrenResponse
		if decErr := json.NewDecoder(resp.Body).Decode(&lcr); decErr != nil {
			return childrenPage{}, fmt.Errorf("graph: decoding children response: %w", decErr)
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
				return childrenPage{}, stripErr
			}
		}

		return childrenPage{items: items, nextPath: nextPath}, nil
	}

	if !isExactRootChildrenCollectionPath(path) {
		result, err := op()
		if err != nil {
			return nil, "", err
		}

		return result.items, result.nextPath, nil
	}

	result, err := doDocumentedGraphQuirkRetry(ctx, c, documentedGraphQuirkSpec{
		name:   QuirkRootChildrenTransient404,
		policy: c.policies.rootChildren,
		match:  isTransientRootChildrenError,
	}, op)
	if err != nil {
		return nil, "", err
	}

	return result.items, result.nextPath, nil
}

// stripBaseURL removes the client's base URL prefix from a full URL,
// returning the path + query string for use with do().
// Returns an error if the URL doesn't start with the expected base.
func (c *Client) stripBaseURL(fullURL string) (string, error) {
	if !strings.HasPrefix(fullURL, c.baseURL) {
		return "", fmt.Errorf("graph: nextLink URL %q does not match base URL %q", fullURL, c.baseURL)
	}

	return fullURL[len(c.baseURL):], nil
}
