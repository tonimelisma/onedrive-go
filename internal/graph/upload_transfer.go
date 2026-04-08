package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// SimpleUpload uploads a file up to 4 MB using a single PUT request.
// For larger files, use CreateUploadSession + UploadChunk.
// The content is sent with application/octet-stream content type.
func (c *Client) SimpleUpload(
	ctx context.Context, driveID driveid.ID, parentID, name string, r io.Reader, size int64,
) (*Item, error) {
	c.logger.Info("simple upload",
		slog.String("drive_id", driveID.String()),
		slog.String("parent_id", parentID),
		slog.String("name", name),
		slog.Int64("size", size),
	)

	// Explicit conflictBehavior=replace prevents reliance on undocumented API
	// defaults. CreateUploadSession passes this in the JSON body; SimpleUpload
	// must use a query parameter since the body is the raw file content.
	path := fmt.Sprintf("/drives/%s/items/%s:/%s:/content?@microsoft.graph.conflictBehavior=replace", driveID, parentID, url.PathEscape(name))

	resp, err := c.doRawUpload(ctx, http.MethodPut, path, "application/octet-stream", r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var dir driveItemResponse
	if decErr := json.NewDecoder(resp.Body).Decode(&dir); decErr != nil {
		return nil, fmt.Errorf("graph: decoding simple upload response: %w", decErr)
	}

	item := dir.toItem(c.logger)
	normalizeSingleItem(&item, c.logger)

	return &item, nil
}

// SimpleUploadToItem overwrites an existing file by item ID using a single PUT
// request. Files larger than 4 MiB should use UploadToItem or
// CreateUploadSessionForItem.
func (c *Client) SimpleUploadToItem(
	ctx context.Context, driveID driveid.ID, itemID string, r io.Reader, size int64,
) (*Item, error) {
	c.logger.Info("simple upload existing item",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", itemID),
		slog.Int64("size", size),
	)

	path := fmt.Sprintf("/drives/%s/items/%s/content", driveID, itemID)

	resp, err := c.doRawUpload(ctx, http.MethodPut, path, "application/octet-stream", r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var dir driveItemResponse
	if decErr := json.NewDecoder(resp.Body).Decode(&dir); decErr != nil {
		return nil, fmt.Errorf("graph: decoding simple upload response: %w", decErr)
	}

	item := dir.toItem(c.logger)
	normalizeSingleItem(&item, c.logger)

	return &item, nil
}

// UploadChunk uploads a chunk of data to an upload session.
// Returns the completed Item plus complete=true on the final chunk (201/200).
// For intermediate chunks (202), it returns complete=false with a nil item.
// offset is the byte offset, length is the chunk size, total is the full file size.
// The session URL is pre-authenticated, so no Authorization header is sent.
// chunk must be an io.ReaderAt — each retry creates a fresh SectionReader to avoid
// racing with the HTTP transport's writeLoop goroutine from a previous attempt.
func (c *Client) UploadChunk(
	ctx context.Context, session *UploadSession, chunk io.ReaderAt,
	offset, length, total int64,
) (*Item, bool, error) {
	c.logger.Debug("uploading chunk",
		slog.Int64("offset", offset),
		slog.Int64("length", length),
		slog.Int64("total", total),
	)

	contentRange := fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, total)

	resp, err := c.doPreAuth(ctx, "upload chunk", func() (*http.Request, error) {
		// Fresh SectionReader per attempt — io.ReaderAt.ReadAt is goroutine-safe,
		// so no race with a previous attempt's transport writeLoop goroutine.
		reader := io.NewSectionReader(chunk, 0, length)
		uploadURL, urlErr := c.validatedUploadURL(session.UploadURL)
		if urlErr != nil {
			return nil, urlErr
		}

		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, reader)
		if reqErr != nil {
			return nil, fmt.Errorf("graph: creating chunk upload request: %w", reqErr)
		}

		req.Header.Set("Content-Range", contentRange)
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("User-Agent", c.userAgent)
		req.ContentLength = length

		return req, nil
	})
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	return c.handleChunkResponse(resp)
}

// handleChunkResponse processes the HTTP response from an upload chunk request.
// doPreAuth guarantees only 2xx responses reach this function — non-2xx
// (including 416 Range Not Satisfiable) are handled by doPreAuth and
// returned as *GraphError with appropriate sentinels (e.g., ErrRangeNotSatisfiable).
// 202 Accepted means intermediate chunk; 200/201 means upload complete with item data.
func (c *Client) handleChunkResponse(resp *http.Response) (*Item, bool, error) {
	switch resp.StatusCode {
	case http.StatusAccepted:
		// Intermediate chunk accepted. Drain body to reuse connection.
		if _, drainErr := io.Copy(io.Discard, resp.Body); drainErr != nil {
			return nil, false, fmt.Errorf("graph: draining chunk response body: %w", drainErr)
		}

		c.logger.Debug("intermediate chunk accepted")

		return nil, false, nil

	case http.StatusOK, http.StatusCreated:
		// Upload complete — response contains the created/updated item.
		var dir driveItemResponse
		if decErr := json.NewDecoder(resp.Body).Decode(&dir); decErr != nil {
			return nil, false, fmt.Errorf("graph: decoding final chunk response: %w", decErr)
		}

		item := dir.toItem(c.logger)
		normalizeSingleItem(&item, c.logger)

		c.logger.Debug("upload complete",
			slog.String("item_id", item.ID),
			slog.String("item_name", item.Name),
		)

		return &item, true, nil

	default:
		// Unexpected 2xx status (e.g., 204, 206). doPreAuth filters non-2xx.
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrBodySize))
		if readErr != nil {
			return nil, false, fmt.Errorf("graph: reading unexpected chunk response body: %w", readErr)
		}

		c.logger.Error("chunk upload returned unexpected 2xx status",
			slog.Int("status", resp.StatusCode),
		)

		return nil, false, fmt.Errorf("graph: chunk upload unexpected status %d: %s", resp.StatusCode, string(body))
	}
}

// doRawUpload sends an authenticated request with a custom content type.
// Used for SimpleUpload where application/octet-stream is needed instead of application/json.
// Unlike do(), this does not retry — retrying a partially-consumed reader is not safe.
func (c *Client) doRawUpload(
	ctx context.Context, method, path, contentType string, body io.Reader,
) (*http.Response, error) {
	url := c.baseURL + path

	c.logger.Debug("preparing raw upload request",
		slog.String("method", method),
		slog.String("path", path),
		slog.String("content_type", contentType),
	)

	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("graph: creating raw upload request: %w", err)
	}

	tok, err := c.token.Token()
	if err != nil {
		return nil, fmt.Errorf("graph: obtaining token for upload: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.dispatchRequest(req)
	if err != nil {
		c.logger.Error("raw upload request failed",
			slog.String("method", method),
			slog.String("path", path),
			slog.String("error", err.Error()),
		)

		return nil, fmt.Errorf("graph: raw upload request failed: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrBodySize))
		if readErr != nil {
			if closeErr := resp.Body.Close(); closeErr != nil {
				c.logger.Warn("failed to close upload error response body after read failure",
					slog.String("path", path),
					slog.String("error", closeErr.Error()),
				)
			}
			return nil, fmt.Errorf("graph: reading upload error body: %w", readErr)
		}

		if closeErr := resp.Body.Close(); closeErr != nil {
			c.logger.Warn("failed to close upload error response body",
				slog.String("path", path),
				slog.String("error", closeErr.Error()),
			)
		}

		return nil, buildGraphError(
			resp.StatusCode,
			resp.Header.Get("request-id"),
			parseRetryAfter(resp),
			errBody,
		)
	}

	return resp, nil
}

// Upload uploads a file to OneDrive, automatically choosing simple upload for
// files up to 4 MiB or chunked (resumable) upload for larger files. The session
// lifecycle (create, chunk loop, cancel-on-error) is fully encapsulated.
// content must be an io.ReaderAt so that retries can re-read from arbitrary offsets.
// progress may be nil if no progress reporting is needed.
func (c *Client) Upload(
	ctx context.Context, driveID driveid.ID, parentID, name string,
	content io.ReaderAt, size int64, mtime time.Time, progress ProgressFunc,
) (*Item, error) {
	if size <= SimpleUploadMaxSize {
		item, err := c.simpleUploadCreateByParent(ctx, driveID, parentID, name, content, size, mtime, progress)
		if err != nil {
			return nil, err
		}

		return c.finalizeSimpleUpload(ctx, driveID, item, mtime)
	}

	return c.chunkedUploadEncapsulated(ctx, driveID, parentID, name, content, size, mtime, progress)
}

// UploadToItem overwrites an existing file by item ID, automatically choosing
// simple upload or resumable upload by size.
func (c *Client) UploadToItem(
	ctx context.Context, driveID driveid.ID, itemID string,
	content io.ReaderAt, size int64, mtime time.Time, progress ProgressFunc,
) (*Item, error) {
	if size <= SimpleUploadMaxSize {
		r := io.NewSectionReader(content, 0, size)

		item, err := c.SimpleUploadToItem(ctx, driveID, itemID, r, size)
		if err != nil {
			return nil, err
		}

		return c.finalizeSimpleUpload(ctx, driveID, item, mtime)
	}

	return c.chunkedUploadToExistingItem(ctx, driveID, itemID, content, size, mtime, progress)
}

func (c *Client) finalizeSimpleUpload(
	ctx context.Context,
	driveID driveid.ID,
	item *Item,
	mtime time.Time,
) (*Item, error) {
	// Simple upload (PUT /content) cannot include fileSystemInfo in the
	// request body. Post-upload PATCH preserves local mtime on the server,
	// preventing mtime mismatch on the next sync pass.
	if !mtime.IsZero() {
		patched, patchErr := doQuirkRetry(ctx, c, quirkRetrySpec{
			name:   "simple-upload-mtime-transient-404",
			policy: c.simpleUploadMtimePolicy,
			match:  isTransientSimpleUploadMtimeError,
		}, func() (*Item, error) {
			return c.UpdateFileSystemInfo(ctx, driveID, item.ID, mtime)
		})
		if patchErr != nil {
			return nil, fmt.Errorf("graph: setting mtime after simple upload: %w", patchErr)
		}

		return patched, nil
	}

	return item, nil
}

func (c *Client) simpleUploadCreateByParent(
	ctx context.Context,
	driveID driveid.ID,
	parentID string,
	name string,
	content io.ReaderAt,
	size int64,
	mtime time.Time,
	progress ProgressFunc,
) (*Item, error) {
	item, err := c.SimpleUpload(ctx, driveID, parentID, name, io.NewSectionReader(content, 0, size), size)
	if err == nil {
		return item, nil
	}

	if size == 0 || !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	// Graph misreports some shared-folder create denials as 404 on the simple
	// PUT /content route while the equivalent upload-session route returns the
	// correct permission status. Retry that narrower case through
	// createUploadSession so higher layers see the real outcome.
	c.logger.Warn("simple upload returned not found, retrying via upload session",
		slog.String("drive_id", driveID.String()),
		slog.String("parent_id", parentID),
		slog.String("name", name),
		slog.Int64("size", size),
	)

	item, sessionErr := c.chunkedUploadEncapsulated(ctx, driveID, parentID, name, content, size, mtime, progress)
	if sessionErr == nil || !errors.Is(sessionErr, ErrNotFound) {
		return item, sessionErr
	}

	// Live full-suite coverage also shows the inverse ordering quirk: after the
	// parent path is already readable, both the first simple upload and the
	// follow-on createUploadSession can still report itemNotFound for a while.
	// Once the session route has ruled out the shared-folder 403 case, replay
	// the original simple upload under a slightly longer bounded create
	// convergence budget tuned for the final create path, not the permission
	// oracle.
	c.logger.Warn("upload session create still returned not found, retrying simple upload",
		slog.String("drive_id", driveID.String()),
		slog.String("parent_id", parentID),
		slog.String("name", name),
		slog.Int64("size", size),
	)

	return doQuirkRetry(ctx, c, quirkRetrySpec{
		name:   "simple-upload-create-transient-404",
		policy: c.simpleUploadCreatePolicy,
		match:  isTransientSimpleUploadCreateError,
	}, func() (*Item, error) {
		return c.SimpleUpload(ctx, driveID, parentID, name, io.NewSectionReader(content, 0, size), size)
	})
}

// chunkedUploadEncapsulated creates an upload session, uploads all chunks,
// and cancels the session on any error. Fully encapsulates session lifecycle.
func (c *Client) chunkedUploadEncapsulated(
	ctx context.Context, driveID driveid.ID, parentID, name string,
	content io.ReaderAt, size int64, mtime time.Time, progress ProgressFunc,
) (*Item, error) {
	session, err := c.CreateUploadSession(ctx, driveID, parentID, name, size, mtime)
	if err != nil {
		return nil, err
	}

	item, err := c.uploadAllChunks(ctx, session, content, size, progress)
	if err != nil {
		// Best-effort cancel — detach from cancellation so the upload session is
		// cleaned up even when the request context has already been canceled.
		cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()

		cancelErr := c.CancelUploadSession(cancelCtx, session)
		if cancelErr != nil {
			c.logger.Warn("failed to cancel upload session after error",
				slog.String("error", cancelErr.Error()),
			)
		}

		return nil, err
	}

	return item, nil
}

func (c *Client) chunkedUploadToExistingItem(
	ctx context.Context, driveID driveid.ID, itemID string,
	content io.ReaderAt, size int64, mtime time.Time, progress ProgressFunc,
) (*Item, error) {
	session, err := c.CreateUploadSessionForItem(ctx, driveID, itemID, size, mtime)
	if err != nil {
		return nil, err
	}

	item, err := c.uploadAllChunks(ctx, session, content, size, progress)
	if err != nil {
		cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()

		cancelErr := c.CancelUploadSession(cancelCtx, session)
		if cancelErr != nil {
			c.logger.Warn("failed to cancel upload session after error",
				slog.String("error", cancelErr.Error()),
			)
		}

		return nil, err
	}

	return item, nil
}

// uploadAllChunks uploads all chunks of a file to an upload session.
// Returns the completed Item from the final chunk response.
func (c *Client) uploadAllChunks(
	ctx context.Context, session *UploadSession,
	content io.ReaderAt, size int64, progress ProgressFunc,
) (*Item, error) {
	return c.uploadChunksFrom(ctx, session, content, size, 0, progress)
}

// uploadChunksFrom uploads chunks starting at startOffset. Shared by
// uploadAllChunks (startOffset=0) and ResumeUpload (startOffset=resumeOffset).
func (c *Client) uploadChunksFrom(
	ctx context.Context, session *UploadSession,
	content io.ReaderAt, totalSize, startOffset int64, progress ProgressFunc,
) (*Item, error) {
	if totalSize == 0 {
		return nil, fmt.Errorf("graph: cannot upload zero-size file via chunked upload")
	}

	var lastItem *Item

	for offset := startOffset; offset < totalSize; {
		chunkSize := int64(ChunkedUploadChunkSize)
		if offset+chunkSize > totalSize {
			chunkSize = totalSize - offset
		}

		chunk := io.NewSectionReader(content, offset, chunkSize)

		item, complete, err := c.UploadChunk(ctx, session, chunk, offset, chunkSize, totalSize)
		if err != nil {
			return nil, fmt.Errorf("graph: uploading chunk at offset %d: %w", offset, err)
		}

		offset += chunkSize

		if progress != nil {
			progress(offset, totalSize)
		}

		if complete {
			lastItem = item
		}
	}

	if lastItem == nil {
		return nil, fmt.Errorf("graph: upload completed all chunks but received no final item")
	}

	return lastItem, nil
}

// UploadFromSession uploads all chunks for an existing upload session.
// Unlike chunkedUploadEncapsulated, the caller manages session lifecycle
// including cancellation on permanent failure. This allows callers to persist
// session state for resume across process restarts.
func (c *Client) UploadFromSession(
	ctx context.Context, session *UploadSession,
	content io.ReaderAt, totalSize int64, progress ProgressFunc,
) (*Item, error) {
	return c.uploadAllChunks(ctx, session, content, totalSize, progress)
}
