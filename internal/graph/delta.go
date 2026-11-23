package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// deltaResponse mirrors the Graph API delta response JSON structure.
// Unexported — callers receive normalized DeltaPage values.
type deltaResponse struct {
	Value     []driveItemResponse `json:"value"`
	NextLink  string              `json:"@odata.nextLink"`
	DeltaLink string              `json:"@odata.deltaLink"`
}

// ErrDeltaTokenNotALink is a delta token that is neither empty nor a link: a
// token this client saved is always the delta link Graph answered, so one
// that is not is corrupt, and starting over from the initial path would read
// a full enumeration as the changes since the token. A caller recovers as
// from a token it cannot continue.
var ErrDeltaTokenNotALink = errors.New("graph: delta token is not a link")

// deltaPathBuilder constructs the initial API path for a delta request when
// no token is provided.
type deltaPathBuilder func() (string, error)

// driveDeltaPath builds the initial path of a drive's delta.
func driveDeltaPath(driveID driveid.ID) deltaPathBuilder {
	return func() (string, error) {
		return driveRoute(driveID, "/root/delta")
	}
}

// folderDeltaPath builds the initial path of a folder-scoped delta.
func folderDeltaPath(driveID driveid.ID, folderID string) deltaPathBuilder {
	return func() (string, error) {
		return itemRoute(driveID, folderID, "/delta")
	}
}

// Delta fetches one page of delta changes for a drive.
// Pass an empty token for the initial sync (fetches all items).
// For subsequent calls, pass the DeltaLink or NextLink value from the
// previous DeltaPage — these are full URLs that get converted to paths.
// Returns a DeltaPage with normalized items, and either NextLink (more pages)
// or DeltaLink (done). HTTP 410 (Gone) means the token has expired — returns ErrGone.
func (c *Client) Delta(ctx context.Context, driveID driveid.ID, token string) (*DeltaPage, error) {
	return c.fetchDeltaPage(ctx, token, driveDeltaPath(driveID), slog.String("drive_id", driveID.String()))
}

// DeltaFolder fetches one page of delta changes for a specific folder within a drive.
// This is folder-scoped delta: /drives/{driveID}/items/{folderID}/delta.
// Only works on OneDrive Personal — Business/SharePoint only support root-scoped delta.
// Pass an empty token for initial sync. Returns ErrGone on HTTP 410.
func (c *Client) DeltaFolder(ctx context.Context, driveID driveid.ID, folderID, token string) (*DeltaPage, error) {
	return c.fetchDeltaPage(ctx, token, folderDeltaPath(driveID, folderID),
		slog.String("drive_id", driveID.String()), slog.String("folder_id", folderID))
}

// fetchDeltaPage is the shared implementation for Delta and DeltaFolder: it
// resolves the token to an API path and fetches the page there.
func (c *Client) fetchDeltaPage(
	ctx context.Context,
	token string,
	buildPath deltaPathBuilder,
	logAttrs ...slog.Attr,
) (*DeltaPage, error) {
	path, err := c.resolveDeltaToken(token, buildPath)
	if err != nil {
		return nil, err
	}

	return c.fetchDeltaPath(ctx, path, token == "", logAttrs...)
}

// fetchDeltaPath fetches the delta page at path, decodes the response, and
// applies the normalization pipeline. initialSync says whether the page is
// asked for without a token: the first page of an initial sync.
func (c *Client) fetchDeltaPath(
	ctx context.Context,
	path string,
	initialSync bool,
	logAttrs ...slog.Attr,
) (*DeltaPage, error) {
	args := attrsToArgs(logAttrs)
	args = append(args, slog.Bool("initial_sync", initialSync))
	c.logger.Debug("fetching delta page", args...)

	resp, err := c.doGetWithHeaders(ctx, path, c.deltaPreferHeader)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // response body is read-only; its close reports nothing a caller can act on

	var dr deltaResponse
	if err := json.NewDecoder(resp.Body).Decode(&dr); err != nil {
		return nil, fmt.Errorf("graph: decoding delta response: %w", err)
	}

	items := make([]Item, 0, len(dr.Value))
	for i := range dr.Value {
		items = append(items, dr.Value[i].toItem(c.logger))
	}

	// Apply delta-specific normalization pipeline (package filtering,
	// hash clearing, dedup, deletion reordering).
	items = normalizeDeltaItems(items, c.logger)

	c.logger.Debug("fetched delta page",
		slog.Int("raw_count", len(dr.Value)),
		slog.Int("normalized_count", len(items)),
		slog.Bool("has_next_link", dr.NextLink != ""),
		slog.Bool("has_delta_link", dr.DeltaLink != ""),
	)

	page := &DeltaPage{
		Items:     items,
		NextLink:  dr.NextLink,
		DeltaLink: dr.DeltaLink,
		Path:      path,
	}
	if dr.NextLink != "" {
		// A next link that does not resolve leaves NextPath empty; the
		// request for it is refused when it is asked for.
		if nextPath, linkErr := c.stripBaseURL(dr.NextLink); linkErr == nil {
			page.NextPath = nextPath
		}
	}

	return page, nil
}

// resolveDeltaToken converts a delta token into the API path it asks for:
// the initial path for an empty token, the link's path (stripBaseURL) for a
// link, and ErrDeltaTokenNotALink for anything else, never the initial path.
func (c *Client) resolveDeltaToken(token string, buildPath deltaPathBuilder) (string, error) {
	if token == "" {
		return buildPath()
	}
	if !isLink(token) {
		return "", fmt.Errorf("graph: reading delta token: %w", ErrDeltaTokenNotALink)
	}

	path, err := c.stripBaseURL(token)
	if err != nil {
		return "", fmt.Errorf("graph: invalid delta token URL: %w", err)
	}

	return path, nil
}

// isLink reports whether token has the form of a link: an absolute http or
// https URL, the scheme in any letter case, or a relative reference with a
// path ("drives/...", "/v1.0/drives/...").
func isLink(token string) bool {
	ref, err := url.Parse(token)
	if err != nil {
		return false
	}
	if ref.IsAbs() {
		return ref.Scheme == httpScheme || ref.Scheme == httpsScheme
	}

	return strings.Contains(ref.Path, "/")
}

// DeltaAll fetches all pages of delta changes and returns the combined items
// and the new delta token for the next sync run.
// On success, the returned token is always a non-empty DeltaLink.
func (c *Client) DeltaAll(ctx context.Context, driveID driveid.ID, token string) ([]Item, string, error) {
	return c.deltaAllPages(ctx, token, driveDeltaPath(driveID), slog.String("drive_id", driveID.String()))
}

// DeltaFolderAll fetches all pages of folder-scoped delta changes and returns
// the combined items and the new delta token.
// Only works on OneDrive Personal — Business/SharePoint only support root-scoped delta.
func (c *Client) DeltaFolderAll(ctx context.Context, driveID driveid.ID, folderID, token string) ([]Item, string, error) {
	return c.deltaAllPages(ctx, token, folderDeltaPath(driveID, folderID),
		slog.String("drive_id", driveID.String()), slog.String("folder_id", folderID))
}

// deltaAllPages is the shared pagination loop for DeltaAll and DeltaFolderAll:
// it fetches page after page until a DeltaLink is received. askedPages counts
// its pages, keyed on the path each token resolves to. A loop that follows
// next links until a delta link comes can never finish on a link back to a
// page it already asked for, the one just asked included, so such a link fails
// the listing before that page is asked for again; a chain of ever new links
// stops at maxDeltaPages.
func (c *Client) deltaAllPages(
	ctx context.Context,
	token string,
	buildPath deltaPathBuilder,
	logAttrs ...slog.Attr,
) ([]Item, string, error) {
	args := attrsToArgs(logAttrs)
	args = append(args, slog.Bool("initial_sync", token == ""))
	c.logger.Info("starting full delta enumeration", args...)

	var allItems []Item

	pages := newAskedPages("delta enumeration", c.maxDeltaPages)
	currentToken := token

	for {
		path, err := c.resolveDeltaToken(currentToken, buildPath)
		if err != nil {
			return nil, "", err
		}

		page, err := pages.ask(path)
		if err != nil {
			return nil, "", err
		}

		dp, err := c.fetchDeltaPath(ctx, path, currentToken == "", logAttrs...)
		if err != nil {
			return nil, "", err
		}

		allItems = append(allItems, dp.Items...)

		c.logger.Debug("accumulated delta items",
			slog.Int("page", page),
			slog.Int("page_items", len(dp.Items)),
			slog.Int("total_items", len(allItems)),
		)

		// DeltaLink means we have consumed all pages — done.
		if dp.DeltaLink != "" {
			doneArgs := attrsToArgs(logAttrs)
			doneArgs = append(doneArgs,
				slog.Int("total_items", len(allItems)),
				slog.Int("pages", page),
			)
			c.logger.Info("full delta enumeration complete", doneArgs...)

			return allItems, dp.DeltaLink, nil
		}

		// NextLink means more pages — continue with the next page URL as token.
		if dp.NextLink != "" {
			currentToken = dp.NextLink

			continue
		}

		return nil, "", fmt.Errorf("graph: delta page %d has neither nextLink nor deltaLink", page)
	}
}

// attrsToArgs converts slog.Attr values to a slice of any for use with slog methods.
func attrsToArgs(attrs []slog.Attr) []any {
	args := make([]any, 0, len(attrs))
	for _, a := range attrs {
		args = append(args, a)
	}

	return args
}
