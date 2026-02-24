package graph

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ErrNoDownloadURL is returned when a drive item has no pre-authenticated download URL.
// This can happen for folders, OneNote packages, or zero-byte files.
var ErrNoDownloadURL = errors.New("graph: item has no download URL")

// Download streams the content of a drive item to the given writer.
// It first fetches the item metadata to obtain the pre-authenticated download URL,
// then streams the content directly from that URL (bypassing the Graph API).
// Returns the number of bytes written.
func (c *Client) Download(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error) {
	c.logger.Info("downloading item",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", itemID),
	)

	item, err := c.GetItem(ctx, driveID, itemID)
	if err != nil {
		return 0, fmt.Errorf("graph: getting item for download: %w", err)
	}

	if item.DownloadURL == "" {
		// Warn, not Error: this is expected for folders, OneNote packages, and
		// zero-byte files — not a terminal failure requiring investigation.
		c.logger.Warn("item has no download URL",
			slog.String("drive_id", driveID.String()),
			slog.String("item_id", itemID),
			slog.Bool("is_folder", item.IsFolder),
			slog.Bool("is_package", item.IsPackage),
		)

		return 0, ErrNoDownloadURL
	}

	n, err := c.downloadFromURL(ctx, item.DownloadURL, w)
	if err != nil {
		return 0, err
	}

	c.logger.Debug("download complete",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", itemID),
		slog.Int64("bytes_written", n),
	)

	return n, nil
}

// downloadFromURL streams content from a pre-authenticated URL directly to the writer.
// The URL is pre-authenticated by the Graph API, so no Authorization header is needed.
// The URL itself is never logged because it contains embedded auth tokens (architecture.md section 9.2).
// Only the HTTP request/response cycle is retried; streaming (io.Copy) happens after
// doPreAuthRetry returns, so partial-stream failures are handled by the caller.
func (c *Client) downloadFromURL(ctx context.Context, downloadURL string, w io.Writer) (int64, error) {
	resp, err := c.doPreAuthRetry(ctx, "download", func() (*http.Request, error) {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, http.NoBody)
		if reqErr != nil {
			return nil, fmt.Errorf("graph: creating download request: %w", reqErr)
		}

		req.Header.Set("User-Agent", userAgent)

		return req, nil
	})
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	n, copyErr := io.Copy(w, resp.Body)
	if copyErr != nil {
		c.logger.Error("streaming download content failed",
			slog.String("error", copyErr.Error()),
			slog.Int64("bytes_before_error", n),
		)

		return n, fmt.Errorf("graph: streaming download content: %w", copyErr)
	}

	return n, nil
}
