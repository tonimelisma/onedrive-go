package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ChunkAlignment is the required alignment for upload chunk sizes (320 KiB).
// All chunks except the final one must be a multiple of this value.
const ChunkAlignment = 320 * 1024

// SimpleUploadMaxSize is the maximum file size for simple (single-request) upload (4 MiB).
// Files larger than this must use resumable upload sessions.
const SimpleUploadMaxSize = 4 * 1024 * 1024

// ChunkedUploadChunkSize is the chunk size for resumable uploads (10 MiB, aligned to 320 KiB).
const ChunkedUploadChunkSize = 10 * 1024 * 1024

// ProgressFunc is a callback invoked after each chunk upload completes.
// bytesUploaded is cumulative; totalBytes is the full file size.
type ProgressFunc func(bytesUploaded, totalBytes int64)

// Upload session request/response types for Graph API JSON serialization.
type createUploadSessionRequest struct {
	Item uploadSessionItem `json:"item"`
}

type uploadSessionItem struct {
	ConflictBehavior string          `json:"@microsoft.graph.conflictBehavior"` //nolint:tagliatelle // Graph API annotation key
	FileSystemInfo   *fileSystemInfo `json:"fileSystemInfo,omitempty"`
}

// fileSystemInfo preserves local timestamps on upload, preventing OneDrive
// from overwriting them with server-side receipt time (double-versioning).
type fileSystemInfo struct {
	LastModifiedDateTime string `json:"lastModifiedDateTime"`
}

type uploadSessionResponse struct {
	UploadURL          string `json:"uploadUrl"`
	ExpirationDateTime string `json:"expirationDateTime"`
}

// uploadSessionStatusResponse is the JSON shape returned when querying an upload session.
type uploadSessionStatusResponse struct {
	UploadURL          string   `json:"uploadUrl"`
	ExpirationDateTime string   `json:"expirationDateTime"`
	NextExpectedRanges []string `json:"nextExpectedRanges"`
}

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

	path := fmt.Sprintf("/drives/%s/items/%s:/%s:/content", driveID, parentID, url.PathEscape(name))

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

	return &item, nil
}

// CreateUploadSession creates a resumable upload session for a file.
// The returned UploadSession contains a pre-authenticated upload URL.
// When mtime is non-zero, fileSystemInfo is included in the request to
// preserve the local modification timestamp on the server.
func (c *Client) CreateUploadSession(
	ctx context.Context, driveID driveid.ID, parentID, name string, size int64, mtime time.Time,
) (*UploadSession, error) {
	c.logger.Info("creating upload session",
		slog.String("drive_id", driveID.String()),
		slog.String("parent_id", parentID),
		slog.String("name", name),
		slog.Int64("size", size),
	)

	path := fmt.Sprintf("/drives/%s/items/%s:/%s:/createUploadSession", driveID, parentID, url.PathEscape(name))

	item := uploadSessionItem{ConflictBehavior: "replace"}
	if !mtime.IsZero() {
		item.FileSystemInfo = &fileSystemInfo{
			LastModifiedDateTime: mtime.UTC().Format(time.RFC3339),
		}
	}

	reqBody := createUploadSessionRequest{Item: item}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("graph: marshaling upload session request: %w", err)
	}

	resp, err := c.Do(ctx, http.MethodPost, path, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return c.parseUploadSessionResponse(resp)
}

// UploadChunk uploads a chunk of data to an upload session.
// Returns the completed Item on the final chunk (201/200), nil for intermediate chunks (202).
// offset is the byte offset, length is the chunk size, total is the full file size.
// The session URL is pre-authenticated, so no Authorization header is sent.
// chunk must be an io.ReaderAt — each retry creates a fresh SectionReader to avoid
// racing with the HTTP transport's writeLoop goroutine from a previous attempt.
func (c *Client) UploadChunk(
	ctx context.Context, session *UploadSession, chunk io.ReaderAt,
	offset, length, total int64,
) (*Item, error) {
	c.logger.Debug("uploading chunk",
		slog.Int64("offset", offset),
		slog.Int64("length", length),
		slog.Int64("total", total),
	)

	contentRange := fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, total)

	resp, err := c.doPreAuthRetry(ctx, "upload chunk", func() (*http.Request, error) {
		// Fresh SectionReader per attempt — io.ReaderAt.ReadAt is goroutine-safe,
		// so no race with a previous attempt's transport writeLoop goroutine.
		reader := io.NewSectionReader(chunk, 0, length)

		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPut, session.UploadURL, reader)
		if reqErr != nil {
			return nil, fmt.Errorf("graph: creating chunk upload request: %w", reqErr)
		}

		req.Header.Set("Content-Range", contentRange)
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("User-Agent", userAgent)
		req.ContentLength = length

		return req, nil
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return c.handleChunkResponse(resp)
}

// handleChunkResponse processes the HTTP response from an upload chunk request.
// doPreAuthRetry guarantees only 2xx responses reach this function — non-2xx
// (including 416 Range Not Satisfiable) are handled by doPreAuthRetry and
// returned as *GraphError with appropriate sentinels (e.g., ErrRangeNotSatisfiable).
// 202 Accepted means intermediate chunk; 200/201 means upload complete with item data.
func (c *Client) handleChunkResponse(resp *http.Response) (*Item, error) {
	switch resp.StatusCode {
	case http.StatusAccepted:
		// Intermediate chunk accepted. Drain body to reuse connection.
		if _, drainErr := io.Copy(io.Discard, resp.Body); drainErr != nil {
			return nil, fmt.Errorf("graph: draining chunk response body: %w", drainErr)
		}

		c.logger.Debug("intermediate chunk accepted")

		return nil, nil

	case http.StatusOK, http.StatusCreated:
		// Upload complete — response contains the created/updated item.
		var dir driveItemResponse
		if decErr := json.NewDecoder(resp.Body).Decode(&dir); decErr != nil {
			return nil, fmt.Errorf("graph: decoding final chunk response: %w", decErr)
		}

		item := dir.toItem(c.logger)

		c.logger.Debug("upload complete",
			slog.String("item_id", item.ID),
			slog.String("item_name", item.Name),
		)

		return &item, nil

	default:
		// Unexpected 2xx status (e.g., 204, 206). doPreAuthRetry filters non-2xx.
		body, _ := io.ReadAll(resp.Body) //nolint:errcheck // best-effort read for error message
		c.logger.Error("chunk upload returned unexpected 2xx status",
			slog.Int("status", resp.StatusCode),
		)

		return nil, fmt.Errorf("graph: chunk upload unexpected status %d: %s", resp.StatusCode, string(body))
	}
}

// CancelUploadSession cancels an in-progress upload session.
// The session URL is pre-authenticated, so no Authorization header is sent.
func (c *Client) CancelUploadSession(ctx context.Context, session *UploadSession) error {
	c.logger.Info("canceling upload session")

	resp, err := c.doPreAuthRetry(ctx, "cancel upload session", func() (*http.Request, error) {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodDelete, session.UploadURL, http.NoBody)
		if reqErr != nil {
			return nil, fmt.Errorf("graph: creating cancel session request: %w", reqErr)
		}

		req.Header.Set("User-Agent", userAgent)

		return req, nil
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Drain body to reuse connection.
	if _, drainErr := io.Copy(io.Discard, resp.Body); drainErr != nil {
		return fmt.Errorf("graph: draining cancel session response body: %w", drainErr)
	}

	if resp.StatusCode != http.StatusNoContent {
		c.logger.Error("cancel upload session returned unexpected status",
			slog.Int("status", resp.StatusCode),
		)

		return fmt.Errorf("graph: cancel upload session failed with status %d", resp.StatusCode)
	}

	c.logger.Debug("upload session canceled")

	return nil
}

// doRawUpload sends an authenticated request with a custom content type.
// Used for SimpleUpload where application/octet-stream is needed instead of application/json.
// Unlike Do(), this does not retry — retrying a partially-consumed reader is not safe.
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
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logger.Error("raw upload request failed",
			slog.String("method", method),
			slog.String("path", path),
			slog.String("error", err.Error()),
		)

		return nil, fmt.Errorf("graph: raw upload request failed: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		errBody, _ := io.ReadAll(resp.Body) //nolint:errcheck // best-effort read for error message
		resp.Body.Close()

		sentinel := classifyStatus(resp.StatusCode)

		return nil, &GraphError{
			StatusCode: resp.StatusCode,
			RequestID:  resp.Header.Get("request-id"),
			Message:    string(errBody),
			Err:        sentinel,
		}
	}

	return resp, nil
}

// QueryUploadSession queries an upload session's status to determine
// which byte ranges have been accepted. Used for resume after interruption.
// The session URL is pre-authenticated, so no Authorization header is sent.
func (c *Client) QueryUploadSession(
	ctx context.Context, session *UploadSession,
) (*UploadSessionStatus, error) {
	c.logger.Info("querying upload session status")

	resp, err := c.doPreAuthRetry(ctx, "query upload session", func() (*http.Request, error) {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, session.UploadURL, http.NoBody)
		if reqErr != nil {
			return nil, fmt.Errorf("graph: creating query session request: %w", reqErr)
		}

		req.Header.Set("User-Agent", userAgent)

		return req, nil
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// doPreAuthRetry guarantees 2xx here. QueryUploadSession expects exactly 200.
	// Other 2xx codes are unexpected but not worth failing on.

	var ssr uploadSessionStatusResponse
	if decErr := json.NewDecoder(resp.Body).Decode(&ssr); decErr != nil {
		return nil, fmt.Errorf("graph: decoding session status response: %w", decErr)
	}

	expTime, parseErr := time.Parse(time.RFC3339, ssr.ExpirationDateTime)
	if parseErr != nil {
		c.logger.Warn("invalid session status expiration, using zero time",
			slog.String("raw", ssr.ExpirationDateTime),
			slog.String("error", parseErr.Error()),
		)
	}

	status := &UploadSessionStatus{
		UploadURL:          ssr.UploadURL,
		ExpirationTime:     expTime,
		NextExpectedRanges: ssr.NextExpectedRanges,
	}

	c.logger.Debug("upload session status",
		slog.Int("pending_ranges", len(status.NextExpectedRanges)),
	)

	return status, nil
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
		r := io.NewSectionReader(content, 0, size)

		item, err := c.SimpleUpload(ctx, driveID, parentID, name, r, size)
		if err != nil {
			return nil, err
		}

		// Simple upload (PUT /content) cannot include fileSystemInfo in the
		// request body. Post-upload PATCH preserves local mtime on the server,
		// preventing mtime mismatch on the next sync cycle.
		if !mtime.IsZero() {
			patched, patchErr := c.UpdateFileSystemInfo(ctx, driveID, item.ID, mtime)
			if patchErr != nil {
				return nil, fmt.Errorf("graph: setting mtime after simple upload: %w", patchErr)
			}

			return patched, nil
		}

		return item, nil
	}

	return c.chunkedUploadEncapsulated(ctx, driveID, parentID, name, content, size, mtime, progress)
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
		// Best-effort cancel — use background context since ctx may be canceled.
		cancelErr := c.CancelUploadSession(context.Background(), session)
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
	var lastItem *Item

	for offset := int64(0); offset < size; {
		chunkSize := int64(ChunkedUploadChunkSize)
		if offset+chunkSize > size {
			chunkSize = size - offset
		}

		chunk := io.NewSectionReader(content, offset, chunkSize)

		item, err := c.UploadChunk(ctx, session, chunk, offset, chunkSize, size)
		if err != nil {
			return nil, fmt.Errorf("graph: uploading chunk at offset %d: %w", offset, err)
		}

		offset += chunkSize

		if progress != nil {
			progress(offset, size)
		}

		if item != nil {
			lastItem = item
		}
	}

	return lastItem, nil
}

// parseUploadSessionResponse parses the HTTP response from CreateUploadSession.
func (c *Client) parseUploadSessionResponse(resp *http.Response) (*UploadSession, error) {
	var usr uploadSessionResponse
	if decErr := json.NewDecoder(resp.Body).Decode(&usr); decErr != nil {
		return nil, fmt.Errorf("graph: decoding upload session response: %w", decErr)
	}

	expTime, parseErr := time.Parse(time.RFC3339, usr.ExpirationDateTime)
	if parseErr != nil {
		c.logger.Warn("invalid upload session expiration, using zero time",
			slog.String("raw", usr.ExpirationDateTime),
			slog.String("error", parseErr.Error()),
		)
	}

	session := &UploadSession{
		UploadURL:      usr.UploadURL,
		ExpirationTime: expTime,
	}

	c.logger.Debug("upload session created",
		slog.Time("expires", session.ExpirationTime),
	)

	return session, nil
}
