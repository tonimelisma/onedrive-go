package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

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

	resp, err := doDocumentedGraphQuirkRetry(ctx, c, documentedGraphQuirkSpec{
		name:   "upload-session-create-transient-404",
		policy: c.uploadSessionCreatePolicy,
		match:  isTransientUploadSessionCreateError,
	}, func() (*http.Response, error) {
		return c.do(ctx, http.MethodPost, path, bytes.NewReader(bodyBytes))
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return c.parseUploadSessionResponse(resp)
}

// CreateUploadSessionForItem creates a resumable upload session that overwrites
// an existing file identified by item ID.
func (c *Client) CreateUploadSessionForItem(
	ctx context.Context, driveID driveid.ID, itemID string, size int64, mtime time.Time,
) (*UploadSession, error) {
	c.logger.Info("creating upload session for existing item",
		slog.String("drive_id", driveID.String()),
		slog.String("item_id", itemID),
		slog.Int64("size", size),
	)

	path := fmt.Sprintf("/drives/%s/items/%s/createUploadSession", driveID, itemID)

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

	resp, err := c.do(ctx, http.MethodPost, path, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return c.parseUploadSessionResponse(resp)
}

// CancelUploadSession cancels an in-progress upload session.
// The session URL is pre-authenticated, so no Authorization header is sent.
func (c *Client) CancelUploadSession(ctx context.Context, session *UploadSession) error {
	c.logger.Info("canceling upload session")

	resp, err := c.doPreAuth(ctx, "cancel upload session", func() (*http.Request, error) {
		uploadURL, urlErr := c.validatedUploadURL(session.UploadURL)
		if urlErr != nil {
			return nil, urlErr
		}

		req, reqErr := http.NewRequestWithContext(ctx, http.MethodDelete, uploadURL, http.NoBody)
		if reqErr != nil {
			return nil, fmt.Errorf("graph: creating cancel session request: %w", reqErr)
		}

		req.Header.Set("User-Agent", c.userAgent)

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

// QueryUploadSession queries an upload session's status to determine
// which byte ranges have been accepted. Used for resume after interruption.
// The session URL is pre-authenticated, so no Authorization header is sent.
func (c *Client) QueryUploadSession(
	ctx context.Context, session *UploadSession,
) (*UploadSessionStatus, error) {
	c.logger.Info("querying upload session status")

	resp, err := c.doPreAuth(ctx, "query upload session", func() (*http.Request, error) {
		uploadURL, urlErr := c.validatedUploadURL(session.UploadURL)
		if urlErr != nil {
			return nil, urlErr
		}

		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, uploadURL, http.NoBody)
		if reqErr != nil {
			return nil, fmt.Errorf("graph: creating query session request: %w", reqErr)
		}

		req.Header.Set("User-Agent", c.userAgent)

		return req, nil
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// doPreAuth guarantees 2xx here. QueryUploadSession expects exactly 200.
	// Other 2xx codes are unexpected but not worth failing on.

	var ssr uploadSessionStatusResponse
	if decErr := json.NewDecoder(resp.Body).Decode(&ssr); decErr != nil {
		return nil, fmt.Errorf("graph: decoding session status response: %w", decErr)
	}

	uploadURL, err := c.validatedUploadURL(UploadURL(ssr.UploadURL))
	if err != nil {
		return nil, err
	}

	expTime, parseErr := time.Parse(time.RFC3339, ssr.ExpirationDateTime)
	if parseErr != nil {
		c.logger.Warn("invalid session status expiration, using zero time",
			slog.String("raw", ssr.ExpirationDateTime),
			slog.String("error", parseErr.Error()),
		)
	}

	status := &UploadSessionStatus{
		UploadURL:          UploadURL(uploadURL),
		ExpirationTime:     expTime,
		NextExpectedRanges: ssr.NextExpectedRanges,
	}

	c.logger.Debug("upload session status",
		slog.Int("pending_ranges", len(status.NextExpectedRanges)),
	)

	return status, nil
}

// ErrUploadSessionExpired indicates that an upload session is no longer valid
// (Graph API returned 404). The caller should fall back to a fresh upload.
var ErrUploadSessionExpired = errors.New("graph: upload session expired")

// ResumeUpload resumes an interrupted chunked upload from where it left off.
// It queries the session to find the next expected byte offset, then uploads
// the remaining chunks. Returns ErrUploadSessionExpired if the session is no
// longer valid (caller should fall back to fresh upload).
// The caller manages session lifecycle including cancellation on permanent failure.
func (c *Client) ResumeUpload(
	ctx context.Context, session *UploadSession,
	content io.ReaderAt, totalSize int64, progress ProgressFunc,
) (*Item, error) {
	c.logger.Info("attempting to resume upload session")

	status, err := c.QueryUploadSession(ctx, session)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrUploadSessionExpired
		}

		return nil, fmt.Errorf("graph: querying session for resume: %w", err)
	}

	resumeOffset, err := parseNextExpectedRangeStart(status.NextExpectedRanges)
	if err != nil {
		return nil, fmt.Errorf("graph: parsing resume offset from session status: %w", err)
	}

	c.logger.Info("resuming upload from offset",
		slog.Int64("offset", resumeOffset),
		slog.Int64("total", totalSize),
	)

	// Use the session URL from the status response (may differ from original).
	resumeSession := &UploadSession{
		UploadURL:      status.UploadURL,
		ExpirationTime: status.ExpirationTime,
	}

	return c.uploadChunksFrom(ctx, resumeSession, content, totalSize, resumeOffset, progress)
}

func parseNextExpectedRangeStart(ranges []string) (int64, error) {
	if len(ranges) == 0 {
		return 0, errors.New("nextExpectedRanges empty")
	}

	rangeStr := strings.TrimSpace(ranges[0])
	start, _, found := strings.Cut(rangeStr, "-")
	if !found || start == "" {
		return 0, fmt.Errorf("invalid nextExpectedRanges entry %q", rangeStr)
	}

	offset, err := strconv.ParseInt(start, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid nextExpectedRanges entry %q: %w", rangeStr, err)
	}

	if offset < 0 {
		return 0, fmt.Errorf("invalid nextExpectedRanges entry %q: negative offset", rangeStr)
	}

	return offset, nil
}

// parseUploadSessionResponse parses the HTTP response from CreateUploadSession.
func (c *Client) parseUploadSessionResponse(resp *http.Response) (*UploadSession, error) {
	var usr uploadSessionResponse
	if decErr := json.NewDecoder(resp.Body).Decode(&usr); decErr != nil {
		return nil, fmt.Errorf("graph: decoding upload session response: %w", decErr)
	}

	uploadURL, err := c.validatedUploadURL(UploadURL(usr.UploadURL))
	if err != nil {
		return nil, err
	}

	expTime, parseErr := time.Parse(time.RFC3339, usr.ExpirationDateTime)
	if parseErr != nil {
		c.logger.Warn("invalid upload session expiration, using zero time",
			slog.String("raw", usr.ExpirationDateTime),
			slog.String("error", parseErr.Error()),
		)
	}

	session := &UploadSession{
		UploadURL:      UploadURL(uploadURL),
		ExpirationTime: expTime,
	}

	c.logger.Debug("upload session created",
		slog.Time("expires", session.ExpirationTime),
	)

	return session, nil
}
