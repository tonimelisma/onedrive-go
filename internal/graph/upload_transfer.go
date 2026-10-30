package graph

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// SimpleUploadToItem overwrites an existing file by item ID using a single PUT
// request. Files larger than 4 MiB should use UploadToItem or
// CreateUploadSessionForItem.
//
// A non-empty ifMatch makes the overwrite conditional: Graph answers 412
// (ErrPreconditionFailed) when the item changed after the caller's
// preflight (L-02), instead of replacing another writer's edit.
func (c *Client) SimpleUploadToItem(
	ctx context.Context, driveID driveid.ID, itemID, ifMatch string, content io.ReaderAt, size int64,
) (*Item, error) {
	c.logger.Debug("simple upload existing item",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", itemID),
		slog.Int64("size", size),
	)

	path := fmt.Sprintf("/drives/%s/items/%s/content", driveID, itemID)

	resp, err := c.doRawUpload(ctx, http.MethodPut, path, "application/octet-stream", content, size, ifMatch)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // response body is read-only; its close reports nothing a caller can act on

	return c.decodeMutatedItem(ctx, resp.Body, "simple upload", func(ctx context.Context) (*Item, error) {
		return c.GetItem(ctx, driveID, itemID)
	})
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

	// A fragment whose body stops moving is abandoned after the stall
	// timeout and resumed through the session (D-045).
	ctx, watchdog := c.watchTransfer(ctx)
	defer watchdog.stop()

	resp, err := c.doPreAuth(ctx, "upload chunk", func() (*http.Request, error) {
		// Fresh SectionReader per attempt — io.ReaderAt.ReadAt is goroutine-safe,
		// so no race with a previous attempt's transport writeLoop goroutine.
		reader := watchdog.reader(io.NewSectionReader(chunk, 0, length))
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
		return nil, false, watchdog.explain(ctx, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body is read-only; its close reports nothing a caller can act on

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
		// Upload complete — response should contain the created/updated item.
		// Without it the content has still landed: the caller looks it up.
		item, hasItem, decErr := c.decodeItemBody(resp.Body, "final chunk")
		if decErr != nil {
			return nil, false, decErr
		}

		if !hasItem {
			return nil, false, ErrUploadCompletedWithoutItem
		}

		c.logger.Debug("upload complete",
			slog.String("item_id", item.ID),
			slog.String("item_name", item.Name),
		)

		return item, true, nil

	default:
		// Unexpected 2xx status (e.g., 204, 206). doPreAuth filters non-2xx.
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrBodySize))
		if readErr != nil {
			return nil, false, fmt.Errorf("graph: reading unexpected chunk response body: %w", readErr)
		}

		// The raw body stays at Debug: it is server output, not something the
		// returned error should carry to the user.
		c.logger.Debug("chunk upload returned unexpected 2xx status",
			slog.Int("status", resp.StatusCode),
			slog.String("body", string(body)),
		)

		return nil, false, fmt.Errorf("graph: chunk upload unexpected status %d", resp.StatusCode)
	}
}

// doRawUpload sends an authenticated request with a custom content type.
// Used for SimpleUpload where application/octet-stream is needed instead of
// application/json. The interactive clients' RetryTransport does retry this
// PUT, so the request carries an exact Content-Length and a GetBody that
// opens a fresh SectionReader over all size bytes. Without them the request
// went out chunked and a retry after a 5xx sent the already-drained reader:
// zero bytes, which replaced the online file with an empty one (D-033).
func (c *Client) doRawUpload(
	ctx context.Context, method, path, contentType string, content io.ReaderAt, size int64, ifMatch string,
) (*http.Response, error) {
	url := c.baseURL + path

	c.logger.Debug("preparing raw upload request",
		slog.String("method", method),
		slog.String("path", path),
		slog.String("content_type", contentType),
	)

	req, err := http.NewRequestWithContext(ctx, method, url, replayableUploadBody(content, size))
	if err != nil {
		return nil, fmt.Errorf("graph: creating raw upload request: %w", err)
	}

	req.ContentLength = size
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(replayableUploadBody(content, size)), nil
	}

	tok, err := c.token.Token()
	if err != nil {
		return nil, fmt.Errorf("graph: obtaining token for upload: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", c.userAgent)
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}

	resp, err := c.dispatchRequest(req)
	if err != nil {
		// Returned, not logged above Debug: the caller reports the failure
		// once, and a canceled context here is an ordinary shutdown.
		c.logger.Debug("raw upload request failed",
			slog.String("method", method),
			slog.String("path", path),
			slog.Any("error", err),
		)

		return nil, fmt.Errorf("graph: raw upload request failed: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrBodySize))
		if readErr != nil {
			if closeErr := resp.Body.Close(); closeErr != nil {
				c.logger.Debug("failed to close upload error response body after read failure",
					slog.String("path", path),
					slog.Any("error", closeErr),
				)
			}
			return nil, fmt.Errorf("graph: reading upload error body: %w", readErr)
		}

		if closeErr := resp.Body.Close(); closeErr != nil {
			c.logger.Debug("failed to close upload error response body",
				slog.String("path", path),
				slog.Any("error", closeErr),
			)
		}

		graphErr := buildGraphError(
			resp.StatusCode,
			resp.Header.Get("request-id"),
			parseRetryAfter(resp),
			errBody,
		)
		graphErr.AgsDiagnostic = resp.Header.Get(headerAgsDiagnostic)

		return nil, graphErr
	}

	return resp, nil
}

// replayableUploadBody returns a reader over exactly size bytes of content.
// A zero-byte upload sends http.NoBody so the request is not chunked.
func replayableUploadBody(content io.ReaderAt, size int64) io.Reader {
	if size == 0 {
		return http.NoBody
	}

	return io.NewSectionReader(content, 0, size)
}

// UploadToItem overwrites an existing file by item ID. Like Upload, every
// non-empty overwrite goes through a session carrying fileSystemInfo and only
// a zero-byte overwrite is PUT and then PATCHed, with the same
// *MtimeNotAppliedError contract.
//
// A non-empty ifMatch (the eTag the caller's preflight read) makes the
// overwrite conditional on both routes (L-02): a remote edit after the
// preflight answers 412 instead of being replaced.
func (c *Client) UploadToItem(
	ctx context.Context, driveID driveid.ID, itemID, ifMatch string,
	content io.ReaderAt, size int64, mtime time.Time, progress ProgressFunc,
) (*Item, error) {
	if size == 0 {
		item, err := c.SimpleUploadToItem(ctx, driveID, itemID, ifMatch, content, size)
		if err != nil {
			return nil, err
		}

		return c.finalizeSimpleUpload(ctx, driveID, item, mtime)
	}

	return c.chunkedUploadToExistingItem(ctx, driveID, itemID, ifMatch, content, size, mtime, progress)
}

// MtimeNotAppliedError reports a zero-byte upload whose content landed but
// whose follow-on fileSystemInfo PATCH failed. Item is the uploaded item: the
// content is on OneDrive, so callers record it as uploaded rather than
// uploading it again (each retry made one more version), and treat only the
// modified time as not applied.
type MtimeNotAppliedError struct {
	Item *Item
	Err  error
}

func (e *MtimeNotAppliedError) Error() string {
	return "graph: setting mtime after simple upload: " + e.Err.Error()
}

func (e *MtimeNotAppliedError) Unwrap() error {
	return e.Err
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
	if mtime.IsZero() {
		return item, nil
	}

	patched, patchErr := doDocumentedGraphQuirkRetry(ctx, c, documentedGraphQuirkSpec{
		name:   QuirkSimpleUploadMtimeTransient404,
		policy: c.policies.simpleUploadMtime,
		match:  isTransientSimpleUploadMtimeError,
	}, func() (*Item, error) {
		return c.UpdateFileSystemInfo(ctx, driveID, item.ID, mtime)
	})
	if patchErr != nil {
		return item, &MtimeNotAppliedError{Item: item, Err: patchErr}
	}

	return patched, nil
}

// chunkedUploadToExistingItem uploads content through a session on itemID
// (conditional on ifMatch when set) and cancels the session on any error.
func (c *Client) chunkedUploadToExistingItem(
	ctx context.Context, driveID driveid.ID, itemID, ifMatch string,
	content io.ReaderAt, size int64, mtime time.Time, progress ProgressFunc,
) (*Item, error) {
	session, err := c.CreateUploadSessionForItem(ctx, driveID, itemID, ifMatch, size, mtime)
	if err != nil {
		return nil, err
	}

	item, err := c.uploadAllChunks(ctx, session, content, size, progress)
	if err != nil {
		cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()

		cancelErr := c.CancelUploadSession(cancelCtx, session)
		if cancelErr != nil {
			// Best-effort cleanup the user cannot act on; the upload error
			// returned below is the report.
			c.logger.Debug("failed to cancel upload session after error",
				slog.Any("error", cancelErr),
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
			if errors.Is(err, ErrRangeNotSatisfiable) {
				recoveredOffset, recoverErr := c.recoverUploadChunkOffset(ctx, session, offset, totalSize)
				if recoverErr != nil {
					return nil, recoverErr
				}

				offset = recoveredOffset
				if progress != nil {
					progress(offset, totalSize)
				}

				continue
			}

			if offset+chunkSize == totalSize {
				// The last fragment may have committed even though its answer
				// was lost or slow (a large file's commit can outlast the
				// response-header timeout). The caller looks the item up
				// before uploading again (D-036).
				return nil, fmt.Errorf("graph: uploading final chunk at offset %d: %w", offset,
					&FinalFragmentError{Err: err})
			}

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

func (c *Client) recoverUploadChunkOffset(
	ctx context.Context, session *UploadSession, currentOffset, totalSize int64,
) (int64, error) {
	c.logger.Debug("upload chunk range mismatch, querying session status",
		slog.Int64("offset", currentOffset),
	)

	status, err := c.QueryUploadSession(ctx, session)
	if err != nil {
		return 0, fmt.Errorf("graph: querying session status after 416 at offset %d: %w", currentOffset, err)
	}

	recoveredOffset, err := parseNextExpectedRangeStart(status.NextExpectedRanges)
	if err != nil {
		return 0, fmt.Errorf("graph: parsing nextExpectedRanges after 416 at offset %d: %w", currentOffset, err)
	}

	// A recovered offset past the end ended the loop with "no final item"
	// and a confusing error (RCL-044).
	if recoveredOffset > totalSize {
		return 0, fmt.Errorf(
			"graph: upload session nextExpectedRanges %d is past the end of the %d-byte file", recoveredOffset, totalSize)
	}

	if recoveredOffset <= currentOffset {
		return 0, fmt.Errorf(
			"graph: upload session nextExpectedRanges did not advance after 416: current=%d next=%d",
			currentOffset,
			recoveredOffset,
		)
	}

	session.UploadURL = status.UploadURL
	session.ExpirationTime = status.ExpirationTime

	c.logger.Debug("upload session recovered authoritative offset after 416",
		slog.Int64("current_offset", currentOffset),
		slog.Int64("recovered_offset", recoveredOffset),
	)

	return recoveredOffset, nil
}

// FinalFragmentError marks a failure of an upload's last fragment. The
// server may have committed the file anyway, so the upload is uncertain
// rather than failed.
type FinalFragmentError struct {
	Err error
}

func (e *FinalFragmentError) Error() string { return "final fragment: " + e.Err.Error() }

func (e *FinalFragmentError) Unwrap() error { return e.Err }

// IsFinalFragmentUncertain reports whether err is a final-fragment failure
// whose upload may nevertheless have committed.
func IsFinalFragmentUncertain(err error) bool {
	var finalErr *FinalFragmentError
	return errors.As(err, &finalErr)
}

// UploadFromSession uploads all chunks for an existing upload session.
// Unlike chunkedUploadToExistingItem, the caller manages session lifecycle
// including cancellation on permanent failure. This allows callers to persist
// session state for resume across process restarts.
func (c *Client) UploadFromSession(
	ctx context.Context, session *UploadSession,
	content io.ReaderAt, totalSize int64, progress ProgressFunc,
) (*Item, error) {
	return c.uploadAllChunks(ctx, session, content, totalSize, progress)
}
