package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
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
// paged listing's are: a circular continuation would hand the caller the
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

	route, err := itemRoute(driveID, itemID, "")
	if err != nil {
		return nil, err
	}

	return c.fetchItem(ctx, route)
}

// GetItemByPath retrieves the item at remotePath, relative to the drive root.
// Graph's path query can answer an item from another path, so the answer is
// placed before it is returned (readPlacedAtPath): by its own parent path, or,
// when it carries none, by its name and the folder it sits in. An answer the
// answers put elsewhere is ErrNotFound. Answers that do not show whether the
// item is at the path are ErrPathAnswersDisagree, which is not ErrNotFound.
// A read costs one request for each chunk of the path when its answer carries
// its parent path, and at most twice that when it does not, at any depth: a
// path is one chunk unless its escaped form is longer than
// maxPathAddressedLength. The path must NOT have a leading slash and must not
// be empty — these are caller bugs that produce malformed API URLs. Returns
// ErrInvalidPath for both cases. A path holding a name the route cannot carry
// is ErrNameNotPathAddressable, before any request. For root, callers should
// use GetItem with itemID "root" instead.
func (c *Client) GetItemByPath(ctx context.Context, driveID driveid.ID, remotePath string) (*Item, error) {
	answer, err := c.readPlacedAtPath(ctx, driveID, remotePath)
	if err != nil {
		return nil, err
	}
	if !answer.place.atPath() {
		return nil, fmt.Errorf("graph: requested path %q resolved to %s: %w", remotePath, answer.elsewhere, ErrNotFound)
	}

	return answer.item, nil
}

// ErrPathAnswersDisagree reports a read or a listing by path whose answers do
// not agree on the folder at a path: the item sits in one folder, or the
// listing lists one, and the read of that folder's path finds nothing, or
// answers another folder that is held there by its name alone or is itself
// at another place. Such answers do not show whether the item, or the listed
// folder, is at the path. It is not ErrNotFound: a caller that takes
// ErrNotFound as leave to look elsewhere, or to create, would act on one of
// the answers. Answers like these can settle as OneDrive converges after a
// change, so a wait for a path to become visible reads it again.
var ErrPathAnswersDisagree = errors.New("graph: answers for one path disagree")

// answerPlace is where the answers for a path put the item a read of the path
// answered.
type answerPlace int

const (
	// placedByItsPath: the answer's own parent path and name are the path.
	placedByItsPath answerPlace = iota + 1
	// placedInTheRoot: a top-level name, sitting in the drive root, which is
	// read by its ID.
	placedInTheRoot
	// placedByItsName: the answer carries no parent path and its name is the
	// path's last name; which folder holds it is still open.
	placedByItsName
	// placedInItsFolder: placedByItsName, and it sits in the folder the read of
	// the parent's path answered, which that folder's own parent path holds
	// there.
	placedInItsFolder
	// placedByFolderName: placedByItsName, and it sits in the folder the read
	// of the parent's path answered, which carries no parent path and is held
	// there by its name alone.
	placedByFolderName
	// placedElsewhere: the answers put it outside the path.
	placedElsewhere
)

// atPath reports whether the answers put the item at the path.
func (p answerPlace) atPath() bool {
	return p.settled() || p == placedByFolderName
}

// settled reports whether the answers' own parent paths and IDs put the item
// at the path, with no read by path whose answer is held by a name alone
// behind it.
func (p answerPlace) settled() bool {
	return p == placedByItsPath || p == placedInTheRoot || p == placedInItsFolder
}

// pathAnswer is a read by path's answer and where the answers put it.
type pathAnswer struct {
	item  *Item
	place answerPlace
	// elsewhere says where the answers put the item, for placedElsewhere.
	elsewhere string
}

// answeredElsewhere is item placedElsewhere, where says where.
func answeredElsewhere(item *Item, where string) pathAnswer {
	return pathAnswer{item: item, place: placedElsewhere, elsewhere: where}
}

// readPlacedAtPath reads remotePath and places its answer: by the answer
// alone when it can (readItemAtPath), otherwise by the folder that holds the
// path (placeByParent).
func (c *Client) readPlacedAtPath(ctx context.Context, driveID driveid.ID, remotePath string) (pathAnswer, error) {
	answer, err := c.readItemAtPath(ctx, driveID, remotePath)
	if err != nil || answer.place != placedByItsName {
		return answer, err
	}

	c.logger.Debug("path lookup missing exact parent path, placing the answer by its parent's ID",
		slog.String("requested_path", remotePath),
		slog.String("resolved_leaf", answer.item.Name),
	)

	return c.placeByParent(ctx, driveID, remotePath, answer.item)
}

// readItemAtPath reads the item at remotePath, one request for each chunk of
// the path, and places the answer as far as the answer itself allows. When it
// carries its parent path, that path plus its name is the asked one
// (namekey.EqualPath, placedByItsPath) or another place (placedElsewhere).
// When it does not, its name is the asked leaf (placedByItsName: which folder
// holds it is still open) or another name (placedElsewhere).
func (c *Client) readItemAtPath(ctx context.Context, driveID driveid.ID, remotePath string) (pathAnswer, error) {
	if err := validateRemotePath(remotePath); err != nil {
		return pathAnswer{}, err
	}
	if err := refuseUnaddressableNames(remotePath); err != nil {
		return pathAnswer{}, err
	}

	c.logger.Debug("getting item by path",
		slog.String("drive_id", driveID.String()),
		slog.String("path", remotePath),
	)

	item, err := c.fetchItemByPathChunks(ctx, driveID, remotePath)
	if err != nil {
		return pathAnswer{}, err
	}

	if exactPath, ok := itemExactRootRelativePath(item); ok {
		if namekey.EqualPath(exactPath, remotePath) {
			return pathAnswer{item: item, place: placedByItsPath}, nil
		}

		return answeredElsewhere(item, strconv.Quote(exactPath)), nil
	}

	if !namekey.Equal(item.Name, pathLeaf(remotePath)) {
		return answeredElsewhere(item, strconv.Quote(itemBestEffortRootRelativePath(item))), nil
	}

	return pathAnswer{item: item, place: placedByItsName}, nil
}

// placeByParent places item, the answer for remotePath that carries no parent
// path and has the path's last name, by the folder that holds remotePath,
// with one more read.
//
// For a top-level name that folder is the drive root, read by its alias (an
// answer names its parent by the root's own ID, never by the alias): item is
// placedInTheRoot when it sits there, and placedElsewhere when it does not.
//
// Below the root the folder is read once by its own path (readItemAtPath) and
// not placed by its own parent in turn: that would cost a request for each
// folder above it, on every read of a drive whose answers carry no parent
// path. item is placedElsewhere when the answers put it outside the path:
// the folder it sits in is answered at another place, or another folder is
// answered at the parent's path by its own parent path. It sits in the folder
// answered at the parent's path, held there by that folder's parent path
// (placedInItsFolder) or, lacking one, its name (placedByFolderName). Any other
// answers do not show whether item is at the path: the read of the parent's
// path finds nothing, answers a folder of the parent's name that is not
// item's, or answers another folder that is itself at another place. That is
// ErrPathAnswersDisagree. When the folder's answer carries no parent path,
// item is held by that folder's name alone, so where every answer agrees on a
// folder of the right name in the wrong place two or more levels up (the path
// route resolving the path into another folder's subtree), item is that
// folder's item, and nothing here catches it.
func (c *Client) placeByParent(ctx context.Context, driveID driveid.ID, remotePath string, item *Item) (pathAnswer, error) {
	cut := strings.LastIndex(remotePath, "/")
	if cut < 0 {
		root, err := c.GetItem(ctx, driveID, driveRootAlias)
		switch {
		case err != nil:
			return pathAnswer{}, fmt.Errorf("graph: reading the drive root that holds %q: %w", remotePath, err)
		case root.ID == "":
			return pathAnswer{}, fmt.Errorf("graph: the drive root that holds %q answered without an item ID", remotePath)
		case !itemSitsIn(item, driveID, root.ID):
			return answeredElsewhere(item, fmt.Sprintf("%q in folder %s, not in the drive root %s", item.Name, item.ParentID, root.ID)), nil
		}

		return pathAnswer{item: item, place: placedInTheRoot}, nil
	}

	if item.ParentID == "" {
		return pathAnswer{}, fmt.Errorf("graph: the read of %q answered %q without its folder", remotePath, item.Name)
	}

	folderPath := remotePath[:cut]
	folder, err := c.readItemAtPath(ctx, driveID, folderPath)
	switch {
	case errors.Is(err, ErrNotFound):
		return pathAnswer{}, fmt.Errorf("graph: %q was read in folder %s, and the read of %q found nothing there (%s): %w",
			remotePath, item.ParentID, folderPath, err.Error(), ErrPathAnswersDisagree)
	case err != nil:
		return pathAnswer{}, fmt.Errorf("graph: reading the folder of %q: %w", remotePath, err)
	case folder.item.ID == "":
		return pathAnswer{}, fmt.Errorf("graph: the folder of %q answered without an item ID", remotePath)
	}

	return placedByFolderAnswer(driveID, remotePath, folderPath, item, folder)
}

// placedByFolderAnswer is item, the answer for remotePath without a parent
// path, placed by folder, the answer of the read at folderPath, the path's
// parent (placeByParent).
func placedByFolderAnswer(driveID driveid.ID, remotePath, folderPath string, item *Item, folder pathAnswer) (pathAnswer, error) {
	inFolder := itemSitsIn(item, driveID, folder.item.ID)
	switch {
	case inFolder && folder.place == placedElsewhere:
		return answeredElsewhere(item, fmt.Sprintf("%q in folder %s, which is at %s", item.Name, item.ParentID, folder.elsewhere)), nil
	case inFolder && folder.place == placedByItsPath:
		return pathAnswer{item: item, place: placedInItsFolder}, nil
	case inFolder:
		return pathAnswer{item: item, place: placedByFolderName}, nil
	case folder.place == placedByItsPath:
		return answeredElsewhere(item,
			fmt.Sprintf("%q in folder %s, while folder %s is at %q", item.Name, item.ParentID, folder.item.ID, folderPath)), nil
	case folder.place == placedElsewhere:
		return pathAnswer{}, fmt.Errorf("graph: %q was read in folder %s, and the read of %q answered folder %s, which is at %s: %w",
			remotePath, item.ParentID, folderPath, folder.item.ID, folder.elsewhere, ErrPathAnswersDisagree)
	default:
		return pathAnswer{}, fmt.Errorf("graph: %q was read in folder %s, and the read of %q answered folder %s: %w",
			remotePath, item.ParentID, folderPath, folder.item.ID, ErrPathAnswersDisagree)
	}
}

// driveRootAlias is the item ID Graph takes for a drive's root folder. It
// names no item: an answer carries the root's own ID as its parent.
const driveRootAlias = "root"

// ErrNameNotPathAddressable reports a name a path-addressed request cannot
// carry (NameIsPathAddressable), or, for a read of one name, one whose
// escaped form is longer than maxPathAddressedLength. A caller that must
// still find such a name lists its folder.
var ErrNameNotPathAddressable = errors.New("graph: name cannot be carried in a path-addressed request")

// Names the path route is known not to resolve, measured on a Personal drive
// on 2026-11-15 (graph-api-quirks.md): a name that starts with the lowercase
// "v1.0" (v1.0, v1.0x, v1.0.zip, v1.0.3; V1.0 and v2.0 resolve) answers 404
// at its own path and at every path through it, and the name "$value" exactly
// answers 400. Both are listed by their folder.
const (
	unresolvedNamePrefix = "v1.0"
	unresolvedName       = "$value"
)

// NameIsPathAddressable reports whether the path route can resolve the name:
// not one holding ':', which ends the path in a path route and which
// url.PathEscape leaves as it is (measured on a Personal drive: Graph answers
// 400 "Resource not found for the segment" for the path cut at the colon),
// not one starting with "v1.0", and not "$value". A caller that walks a path
// by name lists the folder of a name this refuses instead of reading it.
func NameIsPathAddressable(name string) bool {
	return !strings.Contains(name, ":") &&
		!strings.HasPrefix(name, unresolvedNamePrefix) &&
		name != unresolvedName
}

// refuseUnaddressableNames is ErrNameNotPathAddressable for a path, or one
// name, any of whose names the path route cannot resolve
// (NameIsPathAddressable), and nil otherwise. Every read by path or by name
// calls it before its first request.
func refuseUnaddressableNames(remotePath string) error {
	for _, name := range strings.Split(remotePath, "/") {
		if !NameIsPathAddressable(name) {
			return fmt.Errorf("graph: reading %q: %w", remotePath, ErrNameNotPathAddressable)
		}
	}

	return nil
}

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
	if err := refuseUnaddressableNames(name); err != nil {
		return nil, err
	}
	encoded := url.PathEscape(name)
	if len(encoded) > maxPathAddressedLength {
		return nil, fmt.Errorf("graph: reading %q: %w", name, ErrNameNotPathAddressable)
	}

	c.logger.Debug("getting child by name",
		slog.String("drive_id", driveID.String()),
		slog.String("parent_id", parentID),
		slog.String("name", name),
	)

	route, err := childPathRoute(driveID, parentID, encoded)
	if err != nil {
		return nil, err
	}
	item, err := c.fetchItem(ctx, route)
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
func childPathRoute(driveID driveid.ID, parentID, encodedPath string) (string, error) {
	return itemRoute(driveID, parentID, ":/"+encodedPath+":")
}

// childrenRoute is the route of the first page of the folder parentID's
// children.
func childrenRoute(driveID driveid.ID, parentID string) (string, error) {
	return itemRoute(driveID, parentID, fmt.Sprintf("/children?$top=%d", listChildrenPageSize))
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

	route, err := driveRoute(driveID, "/root:/"+chunks[0]+":")
	if err != nil {
		return nil, err
	}
	item, err := c.fetchItem(ctx, route)
	if err != nil {
		return nil, err
	}
	for _, chunk := range chunks[1:] {
		route, err = childPathRoute(driveID, item.ID, chunk)
		if err != nil {
			return nil, err
		}
		item, err = c.fetchItem(ctx, route)
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
	route, err := childrenRoute(driveID, parentID)
	if err != nil {
		return err
	}

	return c.visitChildPages(
		ctx,
		route,
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

// WithoutRepeats returns items with each item ID kept once, at its first
// position: a paged listing can repeat an item across pages, and a lookup by
// name among siblings would count the repeat as a second candidate. Items
// without an ID are all kept. items is not modified.
func WithoutRepeats(items []Item) []Item {
	seen := make(map[string]bool, len(items))
	kept := make([]Item, 0, len(items))
	for i := range items {
		if id := items[i].ID; id != "" {
			if seen[id] {
				continue
			}
			seen[id] = true
		}
		kept = append(kept, items[i])
	}

	return kept
}

// NameSeek finds one name in a folder's listing read a page at a time: its
// Visit is the listing's ChildrenPageFunc. It keeps the children of names
// OneDrive treats as the same (namekey.Equal), each item once however often
// the pages repeat it, and stops the listing at the page that holds the name
// as spelled, so a name near the top of a large folder costs one page. Match
// then picks among them by namekey.Find: a later page could hold the exact
// spelling, and the listing order decides nothing.
type NameSeek struct {
	name       string
	candidates []Item
	seenIDs    map[string]bool
	exact      bool
}

// NewNameSeek returns a NameSeek for name.
func NewNameSeek(name string) *NameSeek {
	return &NameSeek{name: name, seenIDs: make(map[string]bool)}
}

// Visit takes one page of the listing and reports whether to read on.
func (s *NameSeek) Visit(page []Item) bool {
	for i := range page {
		child := &page[i]
		if !namekey.Equal(child.Name, s.name) {
			continue
		}
		if child.ID != "" {
			if s.seenIDs[child.ID] {
				continue
			}
			s.seenIDs[child.ID] = true
		}
		s.candidates = append(s.candidates, *child)
		s.exact = s.exact || child.Name == s.name
	}

	return !s.exact
}

// Candidates are the children that hold the name, a name OneDrive treats as
// the same, in the order listed, each once: for a caller whose own rule picks
// among them (the create conflict of sync, a created folder's read-back).
func (s *NameSeek) Candidates() []Item {
	return s.candidates
}

// Match returns the child that holds the name, when one does, and the
// lookup's answer: Ambiguous when more than one child holds it and none is
// spelled as asked.
func (s *NameSeek) Match() (*Item, namekey.Match) {
	match := namekey.Find(s.candidates, s.name, func(child *Item) (string, bool) { return child.Name, true })
	if !match.Found() {
		return nil, match
	}

	return &s.candidates[match.Index], match
}

// ListChildrenByPathPages is ListChildrenPages for a folder identified by
// path. Its route resolves the path as a read's does, and a read by path can
// answer an item from another path, so the children are held to the path as
// a read is. A path too long for one request is resolved by GetItemByPath and
// its folder listed by ID. A shorter one is listed by path, and each page is
// held to the path before it is handed on (heldToListedPath): a page whose
// answers place a child in another folder, or do not place it, ends the
// listing. The path must NOT have a leading slash and must not be empty —
// returns ErrInvalidPath for both cases, and ErrNameNotPathAddressable for a
// name the route cannot carry, before any request. For root, callers should
// use ListChildrenPages with parentID "root" instead.
func (c *Client) ListChildrenByPathPages(ctx context.Context, driveID driveid.ID, remotePath string, visit ChildrenPageFunc) error {
	if err := validateRemotePath(remotePath); err != nil {
		return err
	}
	if err := refuseUnaddressableNames(remotePath); err != nil {
		return err
	}

	logAttrs := []slog.Attr{
		slog.String("drive_id", driveID.String()),
		slog.String("remote_path", remotePath),
	}

	encodedPath := encodePathSegments(remotePath)
	if len(encodedPath) > maxPathAddressedLength {
		folder, err := c.GetItemByPath(ctx, driveID, remotePath)
		if err != nil {
			return err
		}

		route, err := childrenRoute(driveID, folder.ID)
		if err != nil {
			return err
		}

		return c.visitChildPages(ctx, route,
			"listing children by path", "listed children by path complete", logAttrs, visit)
	}

	route, err := driveRoute(driveID, fmt.Sprintf("/root:/%s:/children?$top=%d", encodedPath, listChildrenPageSize))
	if err != nil {
		return err
	}

	listed := listedPath{driveID: driveID, path: remotePath}
	var misplaced error
	err = c.visitChildPages(ctx, route,
		"listing children by path", "listed children by path complete", logAttrs,
		func(page []Item) bool {
			if misplaced = c.heldToListedPath(ctx, &listed, page); misplaced != nil {
				return false
			}

			return visit(page)
		})
	if err != nil {
		return err
	}

	return misplaced
}

// listedPath is the folder a listing by path asked for, and the answer of the
// read at that path once a child without a parent path needed it.
type listedPath struct {
	driveID driveid.ID
	path    string
	folder  *pathAnswer
}

// heldToListedPath is nil when the answers place every child of page in the
// folder at listed.path, and the error that ends the listing otherwise.
//
// A child that carries its parent path must name that path
// (namekey.EqualPath); one that names another says the route listed another
// folder, which is ErrNotFound for the folder at the path, as a read's answer
// from another path is.
//
// A child that does not carry it is held by the read of the path
// (readPlacedAtPath, as GetItemByPath places it), which is read on the first
// such child and only then, once for the whole listing: a listing whose
// children carry their parent path costs no request more. The child is in the
// folder at the path when it sits in the folder that read answered and the
// answers put that folder at the path. The answers put it elsewhere, and the
// listing ends with ErrNotFound, when it sits in a folder they put at another
// place, or the folder they put at the path by its own parent path and IDs is
// another one. Any other answers do not show whether the route listed the
// folder at the path: the read finds nothing there, or answers another folder
// that is held there by a name alone or is itself at another place. That is
// ErrPathAnswersDisagree. A child that names no folder at all cannot be
// placed, which is an error too. When neither the children nor the folder's
// answer carry a parent path, a listing and a read that agree on a folder of
// the right name in the wrong place two or more levels up are not caught
// (GetItemByPath).
func (c *Client) heldToListedPath(ctx context.Context, listed *listedPath, page []Item) error {
	for i := range page {
		child := &page[i]
		if child.ParentPathKnown {
			if namekey.EqualPath(child.ParentPath, listed.path) {
				continue
			}

			return fmt.Errorf("graph: listing %q answered %q in %q: %w", listed.path, child.Name, child.ParentPath, ErrNotFound)
		}

		if child.ParentID == "" {
			return fmt.Errorf("graph: listing %q answered %q without its folder", listed.path, child.Name)
		}
		if listed.folder == nil {
			folder, err := c.readListedFolder(ctx, listed, child)
			if err != nil {
				return err
			}
			listed.folder = &folder
		}
		if err := listedChildPlaced(listed, child); err != nil {
			return err
		}
	}

	return nil
}

// listedChildPlaced is nil when child, a child of the listing without a parent
// path, is in the folder at listed.path by the answer of the read there, and
// the error that ends the listing otherwise (heldToListedPath).
func listedChildPlaced(listed *listedPath, child *Item) error {
	folder := listed.folder
	inFolder := itemSitsIn(child, listed.driveID, folder.item.ID)
	switch {
	case inFolder && folder.place.atPath():
		return nil
	case inFolder:
		return fmt.Errorf("graph: listing %q answered %q in folder %s, which is at %s: %w",
			listed.path, child.Name, child.ParentID, folder.elsewhere, ErrNotFound)
	case folder.place.settled():
		return fmt.Errorf("graph: listing %q answered %q in folder %s, while folder %s is at %q: %w",
			listed.path, child.Name, child.ParentID, folder.item.ID, listed.path, ErrNotFound)
	case folder.place == placedElsewhere:
		return fmt.Errorf("graph: listing %q answered %q in folder %s, and its read answered folder %s, which is at %s: %w",
			listed.path, child.Name, child.ParentID, folder.item.ID, folder.elsewhere, ErrPathAnswersDisagree)
	default:
		return fmt.Errorf("graph: listing %q answered %q in folder %s, and its read answered folder %s: %w",
			listed.path, child.Name, child.ParentID, folder.item.ID, ErrPathAnswersDisagree)
	}
}

// readListedFolder is the answer of the read at listed.path, placed as
// GetItemByPath places it, for child, the first child of the listing that
// carries no parent path. A read that finds nothing there disagrees with the
// listing, which answered child at that path.
func (c *Client) readListedFolder(ctx context.Context, listed *listedPath, child *Item) (pathAnswer, error) {
	c.logger.Debug("listing by path answered a child without its parent path, reading the folder at the path",
		slog.String("remote_path", listed.path),
	)

	folder, err := c.readPlacedAtPath(ctx, listed.driveID, listed.path)
	switch {
	case errors.Is(err, ErrNotFound):
		return pathAnswer{}, fmt.Errorf("graph: listing %q answered %q, and the read of the path found nothing there (%s): %w",
			listed.path, child.Name, err.Error(), ErrPathAnswersDisagree)
	case err != nil:
		return pathAnswer{}, fmt.Errorf("graph: reading the folder listed at %q: %w", listed.path, err)
	case folder.item.ID == "":
		return pathAnswer{}, fmt.Errorf("graph: the folder listed at %q answered without an item ID", listed.path)
	}

	return folder, nil
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

// stripBaseURL makes a continuation link (an @odata.nextLink, a delta link
// or a saved delta token) the path and query do() asks with: the link below
// the client's base URL, with the base removed. Every paged listing makes
// its links paths here, so a page has one key however it was spelled: the
// scheme and host compare without letter case (RFC 3986 section 6.2.2.1), and
// a relative link is resolved against the base URL as a folder, so
// "drives/d/root/delta?token=x" and "/v1.0/drives/..." name the page they
// name below "https://graph.microsoft.com/v1.0". A link that does not lead
// below the base URL, another host or another version included, is refused:
// a continuation off the Graph host would carry the bearer there.
func (c *Client) stripBaseURL(link string) (string, error) {
	resolved, err := c.resolveLink(link)
	if err != nil {
		return "", err
	}
	base := lowerSchemeAndHost(c.baseURL)
	rest, below := strings.CutPrefix(resolved, base)
	if !below || (rest != "" && rest[0] != '/' && rest[0] != '?') {
		return "", fmt.Errorf("graph: nextLink URL %q does not match base URL %q", link, c.baseURL)
	}

	return rest, nil
}

// resolveLink is link as an absolute URL with its scheme and host in lower
// case: a relative link resolved against the base URL taken as a folder.
func (c *Client) resolveLink(link string) (string, error) {
	ref, err := url.Parse(link)
	if err != nil {
		return "", fmt.Errorf("graph: reading continuation link %q: %w", link, err)
	}
	if ref.IsAbs() {
		return lowerSchemeAndHost(link), nil
	}

	base, err := url.Parse(c.baseURL + "/")
	if err != nil {
		return "", fmt.Errorf("graph: reading base URL %q: %w", c.baseURL, err)
	}

	return lowerSchemeAndHost(base.ResolveReference(ref).String()), nil
}

// lowerSchemeAndHost is rawURL with the scheme and the authority before its
// path in lower case, the path and query as they are; a string without
// "://" is returned unchanged.
func lowerSchemeAndHost(rawURL string) string {
	scheme, rest, found := strings.Cut(rawURL, "://")
	if !found {
		return rawURL
	}
	authorityEnd := strings.IndexAny(rest, "/?#")
	if authorityEnd < 0 {
		authorityEnd = len(rest)
	}

	return strings.ToLower(scheme) + "://" + strings.ToLower(rest[:authorityEnd]) + rest[authorityEnd:]
}

// askedPages counts the pages one paged listing asks for, delta enumeration
// included. Every such loop asks it before each request, with the path it is
// about to ask for: a page past maxPages fails the listing, and so does a
// next link back to a page the listing already asked for, before that page is
// asked for again. A circular continuation would otherwise be followed until
// the cap, each page's results handed on or gathered again every time.
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
