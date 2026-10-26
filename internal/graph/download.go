package graph

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ErrNoDownloadURL is returned when a drive item has no pre-authenticated download URL.
// This can happen for folders, OneNote packages, or zero-byte files.
var ErrNoDownloadURL = errors.New("graph: item has no download URL")

// ErrRangeResumeRejected reports a ranged resume the client must not append:
// the item is no longer the version the partial was started against, the
// server ignored the Range (200), or its Content-Range does not start at the
// requested offset or describe the current size. Appending any of those to
// the partial spliced two versions or two copies of one file, silently for
// a hashless item (D-040). The caller discards the partial and starts over.
var ErrRangeResumeRejected = errors.New("graph: ranged resume rejected")

// ErrUnexpectedDownloadBody reports a download answered with an HTML or JSON
// document for an item that is not one: an error page or a proxy's answer
// with a 200 status. It used to be renamed over the user's file once the hash
// retries ran out, or at once for a hashless item (D-038, D-039).
var ErrUnexpectedDownloadBody = errors.New("graph: download returned an error document instead of the file")

// DownloadVersion binds a ranged resume to the version of the item its
// partial file holds. QuickXorHash is the content identity when the item has
// one; otherwise ETag is.
type DownloadVersion struct {
	ETag         string
	QuickXorHash string
}

// matches reports whether item is still the bound version.
func (v DownloadVersion) matches(item *Item) bool {
	if v.QuickXorHash != "" {
		return item.QuickXorHash == v.QuickXorHash
	}

	return v.ETag != "" && item.ETag == v.ETag
}

// Download streams the content of a drive item to the given writer.
// It first fetches the item metadata to obtain the pre-authenticated download URL,
// then streams the content directly from that URL (bypassing the Graph API).
// Returns the number of bytes written.
func (c *Client) Download(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error) {
	c.logger.Debug("downloading item",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", itemID),
	)

	item, err := c.downloadItemMetadata(ctx, driveID, itemID)
	if err != nil {
		return 0, fmt.Errorf("graph: getting item for download: %w", err)
	}
	if item.MalwareDetected {
		return 0, ErrMalwareDetected
	}

	if item.DownloadURL == "" {
		if !hasDownloadableContent(item) {
			c.logMissingDownloadURL(driveID, itemID, item)
			return 0, ErrNoDownloadURL
		}
		return c.downloadViaContentEndpoint(ctx, driveID, item, w, 0)
	}

	n, err := c.downloadFromURL(ctx, item, w)
	if err != nil {
		if !isPreAuthDownloadUnauthorized(err) {
			return 0, err
		}

		item, err = c.refreshDownloadItemAfterPreAuthUnauthorized(ctx, driveID, itemID)
		if err != nil {
			return 0, err
		}
		if item.DownloadURL == "" {
			if !hasDownloadableContent(item) {
				c.logMissingDownloadURL(driveID, itemID, item)
				return 0, ErrNoDownloadURL
			}
			return c.downloadViaContentEndpoint(ctx, driveID, item, w, 0)
		}

		n, err = c.downloadFromURL(ctx, item, w)
		if isPreAuthDownloadUnauthorized(err) {
			// A fresh tempauth URL refused again: the tempauth scheme itself
			// is unavailable (it retires on 2027-04-01), so use /content.
			return c.downloadViaContentEndpoint(ctx, driveID, item, w, 0)
		}
		if err != nil {
			return 0, err
		}
	}

	c.logger.Debug("download complete",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", itemID),
		slog.Int64("bytes_written", n),
	)

	return n, nil
}

// DownloadRange streams the content of a drive item starting from a byte offset.
// Used to resume interrupted downloads from existing .partial files (B-085).
// version is what the partial holds: when the item is no longer that version,
// or the server answers with anything but a 206 whose Content-Range starts at
// offset and spans the item's size, nothing is written and the error wraps
// ErrRangeResumeRejected. Returns the number of bytes written.
func (c *Client) DownloadRange(
	ctx context.Context, driveID driveid.ID, itemID string, w io.Writer, offset int64, version DownloadVersion,
) (int64, error) {
	c.logger.Debug("downloading item with range",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", itemID),
		slog.Int64("offset", offset),
	)

	item, err := c.downloadItemMetadata(ctx, driveID, itemID)
	if err != nil {
		return 0, fmt.Errorf("graph: getting item for range download: %w", err)
	}
	if item.MalwareDetected {
		return 0, ErrMalwareDetected
	}

	if item.DownloadURL == "" && !hasDownloadableContent(item) {
		c.logMissingDownloadURL(driveID, itemID, item)
		return 0, ErrNoDownloadURL
	}

	if !version.matches(item) {
		return 0, fmt.Errorf("%w: the item changed since the partial download started", ErrRangeResumeRejected)
	}

	if item.DownloadURL == "" {
		return c.downloadViaContentEndpoint(ctx, driveID, item, w, offset)
	}

	n, err := c.downloadFromURLWithRange(ctx, item, w, offset)
	if err != nil {
		if !isPreAuthDownloadUnauthorized(err) {
			return 0, err
		}

		item, err = c.refreshDownloadItemAfterPreAuthUnauthorized(ctx, driveID, itemID)
		if err != nil {
			return 0, err
		}
		if item.DownloadURL == "" {
			c.logMissingDownloadURL(driveID, itemID, item)
			return 0, ErrNoDownloadURL
		}
		if !version.matches(item) {
			return 0, fmt.Errorf("%w: the item changed since the partial download started", ErrRangeResumeRejected)
		}

		n, err = c.downloadFromURLWithRange(ctx, item, w, offset)
		if err != nil {
			return 0, err
		}
	}

	c.logger.Debug("range download complete",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", itemID),
		slog.Int64("bytes_written", n),
		slog.Int64("offset", offset),
	)

	return n, nil
}

func (c *Client) downloadItemMetadata(ctx context.Context, driveID driveid.ID, itemID string) (*Item, error) {
	return doDocumentedGraphQuirkRetry(ctx, c, documentedGraphQuirkSpec{
		name:   QuirkDownloadMetadataTransient404,
		policy: c.policies.downloadMetadata,
		match:  isTransientDownloadMetadataError,
	}, func() (*Item, error) {
		return c.GetItem(ctx, driveID, itemID)
	})
}

func (c *Client) refreshDownloadItemAfterPreAuthUnauthorized(
	ctx context.Context,
	driveID driveid.ID,
	itemID string,
) (*Item, error) {
	c.logger.Debug("refreshing pre-authenticated download URL after unauthorized response",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", itemID),
	)

	item, err := c.downloadItemMetadata(ctx, driveID, itemID)
	if err != nil {
		return nil, fmt.Errorf("graph: refreshing item after unauthorized download URL: %w", err)
	}

	return item, nil
}

func (c *Client) logMissingDownloadURL(driveID driveid.ID, itemID string, item *Item) {
	// Debug only: this is expected for folders, OneNote packages, and
	// zero-byte files. ErrNoDownloadURL is returned, so the caller owns
	// deciding whether it is terminal and reporting it once.
	c.logger.Debug("item has no download URL",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", itemID),
		slog.Bool("is_folder", item != nil && item.IsFolder),
		slog.Bool("is_package", item != nil && item.IsPackage),
	)
}

func isPreAuthDownloadUnauthorized(err error) bool {
	return errors.Is(err, ErrPreAuthURLRejected)
}

// downloadFromURLWithRange streams content from a pre-authenticated URL with
// a Range header. The URL is pre-authenticated, so no Authorization header is
// sent. Only a 206 whose Content-Range starts at offset and spans the item's
// size is appended.
func (c *Client) downloadFromURLWithRange(
	ctx context.Context, item *Item, w io.Writer, offset int64,
) (int64, error) {
	ctx, watchdog := c.watchTransfer(ctx)
	defer watchdog.stop()

	resp, err := c.doPreAuth(ctx, "range download", func() (*http.Request, error) {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, string(item.DownloadURL), http.NoBody)
		if reqErr != nil {
			return nil, fmt.Errorf("graph: creating range download request: %w", reqErr)
		}

		req.Header.Set("User-Agent", c.userAgent)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))

		return req, nil
	})
	if err != nil {
		return 0, watchdog.explain(ctx, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body is read-only; its close reports nothing a caller can act on

	if rangeErr := validateContentRange(resp, offset, item.Size); rangeErr != nil {
		return 0, rangeErr
	}

	if bodyErr := checkDownloadBodyType(resp, item, offset); bodyErr != nil {
		return 0, bodyErr
	}

	n, copyErr := io.Copy(w, watchdog.reader(resp.Body))
	if copyErr != nil {
		// Returned, not logged above Debug: the caller reports the failure
		// once, and a canceled context here is an ordinary shutdown.
		c.logger.Debug("streaming range download content failed",
			slog.String("error", copyErr.Error()),
			slog.Int64("bytes_before_error", n),
		)

		return n, watchdog.explain(ctx, fmt.Errorf("graph: streaming range download content: %w", copyErr))
	}

	return n, nil
}

// validateContentRange is the one check for every resumed download route
// (the tempauth URL and the /content fallback): with offset > 0 it accepts
// only a 206 whose Content-Range is "bytes offset-end/size" for this item. A
// 200 means the server ignored the Range and is sending the whole file,
// which appended to the partial doubled its first bytes; a different start
// or size splices two copies or two versions.
func validateContentRange(resp *http.Response, offset, size int64) error {
	if offset <= 0 {
		return nil
	}

	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("%w: HTTP %d instead of 206", ErrRangeResumeRejected, resp.StatusCode)
	}

	var start, end, total int64
	header := resp.Header.Get("Content-Range")
	if _, err := fmt.Sscanf(header, "bytes %d-%d/%d", &start, &end, &total); err != nil {
		return fmt.Errorf("%w: unreadable Content-Range %q", ErrRangeResumeRejected, header)
	}

	if start != offset || end < start || (size > 0 && (total != size || end != size-1)) {
		return fmt.Errorf("%w: Content-Range %q for offset %d of %d bytes", ErrRangeResumeRejected, header, offset, size)
	}

	return nil
}

// checkDownloadBodyType refuses an HTML or JSON answer that is evidently an
// error document rather than the file. It is only a gate for hashless items:
// a hashed item's bytes are proven or refused by the hash and size checks,
// and a real .json or .html file is served with that type whatever its
// metadata says. For a hashless item the type is evidence only together with
// the size: an answer whose length matches the item is the file.
func checkDownloadBodyType(resp *http.Response, item *Item, offset int64) error {
	if item.QuickXorHash != "" {
		return nil
	}

	served := mediaType(resp.Header.Get("Content-Type"))
	if served != "text/html" && served != "application/json" {
		return nil
	}

	if mediaType(item.MimeType) == served {
		return nil
	}

	if item.Size > 0 && resp.ContentLength >= 0 && offset+resp.ContentLength == item.Size {
		return nil
	}

	return fmt.Errorf("%w (%s for a %s file)", ErrUnexpectedDownloadBody, served, mediaTypeOrUnknown(item.MimeType))
}

func mediaType(contentType string) string {
	parsed, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(contentType))
	}

	return parsed
}

func mediaTypeOrUnknown(contentType string) string {
	if parsed := mediaType(contentType); parsed != "" {
		return parsed
	}

	return "untyped"
}

// downloadFromURL streams content from a pre-authenticated URL directly to the writer.
// The URL is pre-authenticated by the Graph API, so no Authorization header is needed.
// The URL itself is never logged because it contains embedded auth tokens (architecture.md section 9.2).
// Only the HTTP request/response cycle is retried; streaming (io.Copy) happens after
// doPreAuthRetry returns, so partial-stream failures are handled by the caller.
func (c *Client) downloadFromURL(ctx context.Context, item *Item, w io.Writer) (int64, error) {
	const offset = 0

	ctx, watchdog := c.watchTransfer(ctx)
	defer watchdog.stop()

	resp, err := c.doPreAuth(ctx, "download", func() (*http.Request, error) {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, string(item.DownloadURL), http.NoBody)
		if reqErr != nil {
			return nil, fmt.Errorf("graph: creating download request: %w", reqErr)
		}

		req.Header.Set("User-Agent", c.userAgent)

		return req, nil
	})
	if err != nil {
		return 0, watchdog.explain(ctx, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body is read-only; its close reports nothing a caller can act on

	if bodyErr := checkDownloadBodyType(resp, item, offset); bodyErr != nil {
		return 0, bodyErr
	}

	n, copyErr := io.Copy(w, watchdog.reader(resp.Body))
	if copyErr != nil {
		// Returned, not logged above Debug: the caller reports the failure
		// once, and a canceled context here is an ordinary shutdown.
		c.logger.Debug("streaming download content failed",
			slog.String("error", copyErr.Error()),
			slog.Int64("bytes_before_error", n),
		)

		return n, watchdog.explain(ctx, fmt.Errorf("graph: streaming download content: %w", copyErr))
	}

	return n, nil
}

// hasDownloadableContent reports whether a missing downloadUrl means "fetch
// it another way" rather than "there is no content": folders and OneNote
// packages have no file stream.
func hasDownloadableContent(item *Item) bool {
	return item != nil && !item.IsFolder && !item.IsPackage
}

// downloadViaContentEndpoint streams an item through the bearer-authenticated
// GET /items/{id}/content route, the fallback for items without a tempauth
// downloadUrl (that scheme retires on 2027-04-01, RCL-050).
//
// Graph answers with a 302 to a content host. Live probe L-20 (Personal,
// 2026-10-25) showed the content host serves the file without
// Authorization, so the bearer is never sent there: the first leg goes
// through the normal authenticated path (401 forced refresh and transient
// auth handling included) on a client that does not follow redirects, and
// the validated Location is fetched through the unauthenticated pre-auth
// path. With offset > 0 both legs must answer 206 with a Content-Range that
// starts at offset: a 200 carries the whole file, and appending it at the
// offset would corrupt the partial.
func (c *Client) downloadViaContentEndpoint(
	ctx context.Context, driveID driveid.ID, item *Item, w io.Writer, offset int64,
) (int64, error) {
	c.logger.Debug("downloading through the content endpoint",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", item.ID),
	)

	ctx, watchdog := c.watchTransfer(ctx)
	defer watchdog.stop()

	resp, err := c.requestContent(ctx, driveID, item.ID, offset)
	if err != nil {
		return 0, watchdog.explain(ctx, err)
	}

	if resp.StatusCode >= http.StatusMultipleChoices {
		location, locErr := c.validatedContentRedirect(resp)
		if locErr != nil {
			return 0, locErr
		}

		resp, err = c.doPreAuth(ctx, "content download", func() (*http.Request, error) {
			req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, location, http.NoBody)
			if reqErr != nil {
				return nil, fmt.Errorf("graph: creating content download request: %w", reqErr)
			}
			req.Header.Set("User-Agent", c.userAgent)
			if offset > 0 {
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
			}
			return req, nil
		})
		if err != nil {
			return 0, watchdog.explain(ctx, err)
		}
	}
	defer resp.Body.Close() //nolint:errcheck // response body is read-only; its close reports nothing a caller can act on

	if rangeErr := validateContentRange(resp, offset, item.Size); rangeErr != nil {
		return 0, rangeErr
	}

	if bodyErr := checkDownloadBodyType(resp, item, offset); bodyErr != nil {
		return 0, bodyErr
	}

	n, copyErr := io.Copy(w, watchdog.reader(resp.Body))
	if copyErr != nil {
		return n, watchdog.explain(ctx, fmt.Errorf("graph: streaming content download: %w", copyErr))
	}

	return n, nil
}

// requestContent sends the authenticated first leg without following its
// redirect. A 3xx comes back with its body closed; a 2xx is the content.
func (c *Client) requestContent(ctx context.Context, driveID driveid.ID, itemID string, offset int64) (*http.Response, error) {
	var extra http.Header
	if offset > 0 {
		extra = http.Header{"Range": []string{fmt.Sprintf("bytes=%d-", offset)}}
	}

	noFollow := *c.httpClient
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	path := fmt.Sprintf("/drives/%s/items/%s/content", driveID, url.PathEscape(itemID))
	resp, err := c.doRequestVia(ctx, http.MethodGet, path, nil, extra, requestRoute{client: &noFollow, keepRedirect: true})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		if closeErr := resp.Body.Close(); closeErr != nil {
			return nil, fmt.Errorf("graph: closing content redirect response: %w", closeErr)
		}
	}

	return resp, nil
}

// validatedContentRedirect returns the redirect target after checking it is
// a Microsoft content host.
func (c *Client) validatedContentRedirect(resp *http.Response) (string, error) {
	location := resp.Header.Get("Location")
	parsed, err := url.Parse(location)
	if location == "" || err != nil {
		return "", errors.New("graph: content download redirect has no usable Location")
	}

	validate := c.validators.contentRedirect
	if validate == nil {
		validate = validateUploadURL
	}
	if err := validate(parsed); err != nil {
		return "", fmt.Errorf("graph: content download redirect: %w", err)
	}

	return parsed.String(), nil
}
