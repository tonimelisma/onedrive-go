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

// ChildrenPageFunc receives one page of a folder's children, in the order
// OneDrive returns them; the slice is the function's to keep. Returning false
// stops the listing: no further page is asked for.
type ChildrenPageFunc func(page []Item) (keepListing bool)

// visitChildPages is the one loop over the pages of a children listing,
// starting from apiPath, shared by ListChildrenPages, ListChildrenByPathPages
// and ListRecycleBinItems. It hands each page to visit as it arrives and asks
// for the next only after visit returned, so a caller holds one page of a
// folder of any size. A failed page's request is retried as itself, by the
// transport and, for a page asked for at the drive root's children
// collection (the first page, and a later one whose link keeps that form),
// by the documented quirk retry, never from the first page; the pages before
// it have been handed on. Its pages are counted by askedPages, as every
// non-delta listing's are: a circular continuation would hand the caller the
// same children over and over, and a caller that walks them would do its
// work for them again each time.
func (c *Client) visitChildPages(
	ctx context.Context,
	apiPath string,
	entryMsg string,
	doneMsg string,
	logAttrs []slog.Attr,
	visit ChildrenPageFunc,
) error {
	args := attrsToArgs(logAttrs)
	c.logger.Debug(entryMsg, args...)

	handed := 0
	pages := newAskedPages(entryMsg, c.maxListPages)
	for apiPath != "" {
		page, err := pages.ask(apiPath)
		if err != nil {
			return err
		}

		pageItems, nextPath, err := c.listChildrenPage(ctx, apiPath, page)
		if err != nil {
			return err
		}

		handed += len(pageItems)
		if !visit(pageItems) {
			args = append(args, slog.Int("items_handed", handed), slog.Int("pages", page))
			c.logger.Debug(entryMsg+" stopped by its caller", args...)

			return nil
		}

		apiPath = nextPath
	}

	args = append(args, slog.Int("total_items", handed))
	c.logger.Debug(doneMsg, args...)

	return nil
}

// collectChildPages is the slice form of a paged children listing: every
// page, appended in the order it arrived, or nil and the listing's error.
func collectChildPages(list func(visit ChildrenPageFunc) error) ([]Item, error) {
	var items []Item
	if err := list(func(page []Item) bool {
		items = append(items, page...)
		return true
	}); err != nil {
		return nil, err
	}

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

// ListChildrenPages hands the children of the folder parentID to visit a page
// at a time (at most listChildrenPageSize children), in the order OneDrive
// returns them, asking for each page only after visit returned from the one
// before. A caller that only walks the children uses it; visit returning false
// ends the listing without an error. A page that fails ends it with that
// page's error after the pages before it were handed on.
func (c *Client) ListChildrenPages(ctx context.Context, driveID driveid.ID, parentID string, visit ChildrenPageFunc) error {
	return c.visitChildPages(
		ctx,
		fmt.Sprintf("/drives/%s/items/%s/children?$top=%d", driveID, parentID, listChildrenPageSize),
		"listing children",
		"listed children complete",
		[]slog.Attr{
			slog.String("drive_id", driveID.String()),
			slog.String("parent_id", parentID),
		},
		visit,
	)
}

// ListChildren returns every child of a folder, every page read, for a
// caller that needs the whole list (a sort, a lookup by name, one output
// document); a caller that only walks the children uses ListChildrenPages.
func (c *Client) ListChildren(ctx context.Context, driveID driveid.ID, parentID string) ([]Item, error) {
	return collectChildPages(func(visit ChildrenPageFunc) error {
		return c.ListChildrenPages(ctx, driveID, parentID, visit)
	})
}

// ListChildrenByPathPages is ListChildrenPages for a folder identified by
// path. The path must NOT have a leading slash and must not be empty —
// returns ErrInvalidPath for both cases, before any request. For root,
// callers should use ListChildrenPages with parentID "root" instead.
func (c *Client) ListChildrenByPathPages(ctx context.Context, driveID driveid.ID, remotePath string, visit ChildrenPageFunc) error {
	if err := validateRemotePath(remotePath); err != nil {
		return err
	}

	route := fmt.Sprintf("/drives/%s/root:/%s:/children?$top=%d", driveID, encodePathSegments(remotePath), listChildrenPageSize)
	if len(encodePathSegments(remotePath)) > maxPathAddressedLength {
		folder, err := c.fetchItemByPathChunks(ctx, driveID, remotePath)
		if err != nil {
			return err
		}
		route = fmt.Sprintf("/drives/%s/items/%s/children?$top=%d", driveID, folder.ID, listChildrenPageSize)
	}

	return c.visitChildPages(
		ctx,
		route,
		"listing children by path",
		"listed children by path complete",
		[]slog.Attr{
			slog.String("drive_id", driveID.String()),
			slog.String("remote_path", remotePath),
		},
		visit,
	)
}

// ListChildrenByPath is ListChildren for a folder identified by path, with
// the path rules of ListChildrenByPathPages.
func (c *Client) ListChildrenByPath(ctx context.Context, driveID driveid.ID, remotePath string) ([]Item, error) {
	return collectChildPages(func(visit ChildrenPageFunc) error {
		return c.ListChildrenByPathPages(ctx, driveID, remotePath, visit)
	})
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

// askedPages counts the pages one non-delta paged listing asks for. Every
// such loop asks it before each request, with the path it is about to ask
// for: a page past maxPages fails the listing, and so does a next link back
// to a page the listing already asked for, before that page is asked for
// again. A circular continuation would otherwise be followed until the cap,
// each page's results handed on or gathered again every time.
type askedPages struct {
	listing  string
	maxPages int
	// pageOf holds the page number of each path asked for, at most maxPages
	// of them.
	pageOf map[string]int
}

func newAskedPages(listing string, maxPages int) askedPages {
	return askedPages{listing: listing, maxPages: maxPages, pageOf: make(map[string]int)}
}

// ask records apiPath as the listing's next page and returns that page's
// number, or the error that ends the listing without asking for it.
func (p *askedPages) ask(apiPath string) (int, error) {
	page := len(p.pageOf) + 1
	if page > p.maxPages {
		return 0, fmt.Errorf("graph: %s exceeded %d pages", p.listing, p.maxPages)
	}

	if earlier, asked := p.pageOf[apiPath]; asked {
		return 0, fmt.Errorf("graph: %s looped back from page %d to page %d", p.listing, page-1, earlier)
	}

	p.pageOf[apiPath] = page

	return page, nil
}
