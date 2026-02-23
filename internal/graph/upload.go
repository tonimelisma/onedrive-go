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

// chunkAlignment is the required alignment for upload chunk sizes (320 KiB).
// All chunks except the final one must be a multiple of this value.
const chunkAlignment = 320 * 1024

// simpleUploadMaxSize is the maximum file size for simple (single-request) upload (4 MB).
// Files larger than this must use resumable upload sessions.
const simpleUploadMaxSize = 4 * 1024 * 1024

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
func (c *Client) UploadChunk(
	ctx context.Context, session *UploadSession, chunk io.Reader,
	offset, length, total int64,
) (*Item, error) {
	c.logger.Debug("uploading chunk",
		slog.Int64("offset", offset),
		slog.Int64("length", length),
		slog.Int64("total", total),
	)

	contentRange := fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, total)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, session.UploadURL, chunk)
	if err != nil {
		return nil, fmt.Errorf("graph: creating chunk upload request: %w", err)
	}

	req.Header.Set("Content-Range", contentRange)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("User-Agent", userAgent)
	req.ContentLength = length

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logger.Error("chunk upload request failed",
			slog.String("error", err.Error()),
		)

		return nil, fmt.Errorf("graph: chunk upload request failed: %w", err)
	}
	defer resp.Body.Close()

	return c.handleChunkResponse(resp)
}

// handleChunkResponse processes the HTTP response from an upload chunk request.
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

	case http.StatusRequestedRangeNotSatisfiable:
		// 416 means the server has different byte ranges than what we sent.
		// Callers should use QueryUploadSession to discover accepted ranges.
		if _, drainErr := io.Copy(io.Discard, resp.Body); drainErr != nil {
			return nil, fmt.Errorf("graph: draining 416 response body: %w", drainErr)
		}

		c.logger.Warn("upload chunk returned 416 Range Not Satisfiable")

		return nil, ErrRangeNotSatisfiable

	default:
		body, _ := io.ReadAll(resp.Body) //nolint:errcheck // best-effort read for error message
		c.logger.Error("chunk upload failed",
			slog.Int("status", resp.StatusCode),
		)

		return nil, fmt.Errorf("graph: chunk upload failed with status %d: %s", resp.StatusCode, string(body))
	}
}

// CancelUploadSession cancels an in-progress upload session.
// The session URL is pre-authenticated, so no Authorization header is sent.
func (c *Client) CancelUploadSession(ctx context.Context, session *UploadSession) error {
	c.logger.Info("canceling upload session")

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, session.UploadURL, http.NoBody)
	if err != nil {
		return fmt.Errorf("graph: creating cancel session request: %w", err)
	}

	req.Header.Set("User-Agent", userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logger.Error("cancel upload session request failed",
			slog.String("error", err.Error()),
		)

		return fmt.Errorf("graph: cancel upload session request failed: %w", err)
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, session.UploadURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("graph: creating query session request: %w", err)
	}

	req.Header.Set("User-Agent", userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logger.Error("query upload session request failed",
			slog.String("error", err.Error()),
		)

		return nil, fmt.Errorf("graph: query upload session request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body) //nolint:errcheck // best-effort read for error message

		sentinel := classifyStatus(resp.StatusCode)

		return nil, &GraphError{
			StatusCode: resp.StatusCode,
			RequestID:  resp.Header.Get("request-id"),
			Message:    string(body),
			Err:        sentinel,
		}
	}

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
