package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// deltaPreferHeader requests that the Graph API include remote/shared items
// using stable alias IDs in delta responses. Without this header, Personal
// accounts may receive incomplete delta results for shared folders.
// See docs/tier1-research/issues-graph-api-bugs.md.
var deltaPreferHeader = http.Header{
	"Prefer": {"deltashowremoteitemsaliasid"},
}

// deltaResponse mirrors the Graph API delta response JSON structure.
// Unexported — callers receive normalized DeltaPage values.
type deltaResponse struct {
	Value     []driveItemResponse `json:"value"`
	NextLink  string              `json:"@odata.nextLink"`  //nolint:tagliatelle // OData annotation key
	DeltaLink string              `json:"@odata.deltaLink"` //nolint:tagliatelle // OData annotation key
}

// deltaHTTPPrefix is the scheme prefix used to detect full URL tokens
// returned by the Graph API delta endpoint.
const deltaHTTPPrefix = "http"

// maxDeltaPages is the upper bound on pages fetched by DeltaAll/DeltaFolderAll.
// A buggy API or circular NextLinks could loop forever without this guard.
// Package-level var (not const) so tests can temporarily override it.
var maxDeltaPages = 10000 //nolint:gochecknoglobals // test-overridable guard

// deltaPathBuilder constructs the initial API path for a delta request when
// no token (or a non-HTTP token) is provided.
type deltaPathBuilder func() string

// Delta fetches one page of delta changes for a drive.
// Pass an empty token for the initial sync (fetches all items).
// For subsequent calls, pass the DeltaLink or NextLink value from the
// previous DeltaPage — these are full URLs that get converted to paths.
// Returns a DeltaPage with normalized items, and either NextLink (more pages)
// or DeltaLink (done). HTTP 410 (Gone) means the token has expired — returns ErrGone.
func (c *Client) Delta(ctx context.Context, driveID driveid.ID, token string) (*DeltaPage, error) {
	return c.fetchDeltaPage(ctx, token, func() string {
		return fmt.Sprintf("/drives/%s/root/delta", driveID)
	}, slog.String("drive_id", driveID.String()))
}

// DeltaFolder fetches one page of delta changes for a specific folder within a drive.
// This is folder-scoped delta: /drives/{driveID}/items/{folderID}/delta.
// Only works on OneDrive Personal — Business/SharePoint only support root-scoped delta.
// Pass an empty token for initial sync. Returns ErrGone on HTTP 410.
func (c *Client) DeltaFolder(ctx context.Context, driveID driveid.ID, folderID, token string) (*DeltaPage, error) {
	return c.fetchDeltaPage(ctx, token, func() string {
		return fmt.Sprintf("/drives/%s/items/%s/delta", driveID, folderID)
	}, slog.String("drive_id", driveID.String()), slog.String("folder_id", folderID))
}

// fetchDeltaPage is the shared implementation for Delta and DeltaFolder.
// It resolves the token to an API path, fetches one page, decodes the response,
// and applies the normalization pipeline.
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

	args := attrsToArgs(logAttrs)
	args = append(args, slog.Bool("initial_sync", token == ""))
	c.logger.Info("fetching delta page", args...)

	resp, err := c.DoWithHeaders(ctx, http.MethodGet, path, nil, deltaPreferHeader)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

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

	return &DeltaPage{
		Items:     items,
		NextLink:  dr.NextLink,
		DeltaLink: dr.DeltaLink,
	}, nil
}

// resolveDeltaToken converts a delta token into an API path.
// Empty or non-HTTP tokens use the provided path builder for the initial path.
// Full HTTP URL tokens are stripped to relative paths.
func (c *Client) resolveDeltaToken(token string, buildPath deltaPathBuilder) (string, error) {
	if token == "" || !strings.HasPrefix(token, deltaHTTPPrefix) {
		return buildPath(), nil
	}

	path, err := c.stripBaseURL(token)
	if err != nil {
		return "", fmt.Errorf("graph: invalid delta token URL: %w", err)
	}

	return path, nil
}

// buildDeltaPath constructs the API path for a root-scoped delta request.
// Kept for test compatibility — delegates to resolveDeltaToken.
func (c *Client) buildDeltaPath(driveID driveid.ID, token string) (string, error) {
	return c.resolveDeltaToken(token, func() string {
		return fmt.Sprintf("/drives/%s/root/delta", driveID)
	})
}

// buildFolderDeltaPath constructs the API path for a folder-scoped delta request.
// Kept for test compatibility — delegates to resolveDeltaToken.
func (c *Client) buildFolderDeltaPath(driveID driveid.ID, folderID, token string) (string, error) {
	return c.resolveDeltaToken(token, func() string {
		return fmt.Sprintf("/drives/%s/items/%s/delta", driveID, folderID)
	})
}

// DeltaAll fetches all pages of delta changes and returns the combined items
// and the new delta token for the next sync run.
// On success, the returned token is always a non-empty DeltaLink.
func (c *Client) DeltaAll(ctx context.Context, driveID driveid.ID, token string) ([]Item, string, error) {
	return c.deltaAllPages(ctx, token, func(t string) (*DeltaPage, error) {
		return c.Delta(ctx, driveID, t)
	}, slog.String("drive_id", driveID.String()))
}

// DeltaFolderAll fetches all pages of folder-scoped delta changes and returns
// the combined items and the new delta token.
// Only works on OneDrive Personal — Business/SharePoint only support root-scoped delta.
func (c *Client) DeltaFolderAll(ctx context.Context, driveID driveid.ID, folderID, token string) ([]Item, string, error) {
	return c.deltaAllPages(ctx, token, func(t string) (*DeltaPage, error) {
		return c.DeltaFolder(ctx, driveID, folderID, t)
	}, slog.String("drive_id", driveID.String()), slog.String("folder_id", folderID))
}

// deltaAllPages is the shared pagination loop for DeltaAll and DeltaFolderAll.
// It calls fetchPage repeatedly until a DeltaLink is received or maxDeltaPages
// is exceeded.
func (c *Client) deltaAllPages(
	ctx context.Context,
	token string,
	fetchPage func(string) (*DeltaPage, error),
	logAttrs ...slog.Attr,
) ([]Item, string, error) {
	_ = ctx // used by callers' closures; kept in signature for consistency

	args := attrsToArgs(logAttrs)
	args = append(args, slog.Bool("initial_sync", token == ""))
	c.logger.Info("starting full delta enumeration", args...)

	var allItems []Item

	currentToken := token
	page := 1

	for {
		dp, err := fetchPage(currentToken)
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
			page++

			if page > maxDeltaPages {
				return nil, "", fmt.Errorf("graph: delta enumeration exceeded %d pages", maxDeltaPages)
			}

			continue
		}

		// Neither link present — unexpected, but treat as complete with empty token.
		warnArgs := attrsToArgs(logAttrs)
		warnArgs = append(warnArgs, slog.Int("page", page))
		c.logger.Warn("delta response has neither nextLink nor deltaLink", warnArgs...)

		return allItems, "", nil
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
