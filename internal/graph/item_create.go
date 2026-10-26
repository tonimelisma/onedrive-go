package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
)

// Creating a file is two requests: create the empty item with its name in a
// JSON body and conflictBehavior=fail, then send the content by item ID.
//
// The name never travels in a URL. Path routes (items/{parent}:/{name}:/...)
// hand the name to Graph's router: a file named `v1.0` uploaded that way
// created a folder `_api` holding a file `v2.0` (live probe 4), and a path
// route caps names near 291 characters through the URL length limit, where
// the body allows OneDrive's own 400 (L-05).
//
// fail, not replace: replace on create silently overwrote whatever already
// held the name, including a variant OneDrive treats as the same name
// (`STRASSE.txt` over `straße.txt`, `a.txt` over `a😀.txt`) or another
// device's new file. A name that exists answers 409 nameAlreadyExists
// (L-01), which callers turn into a replan.
//
// L-01 measured the cost: the POST makes an empty item that already carries
// the empty-content quickXorHash, and an upload session on that item with
// fileSystemInfo commits one version in total. A zero-byte file needs no
// content step at all.

// createFileFacet marks a JSON create body as a file: an empty "file" object.
type createFileFacet struct{}

type createFileRequest struct {
	Name             string          `json:"name"`
	File             createFileFacet `json:"file"`
	ConflictBehavior string          `json:"@microsoft.graph.conflictBehavior"`
}

// CreateFile creates an empty file named name under parentID. It never
// replaces: an existing item with that name, or with a name OneDrive treats
// as the same, fails with ErrConflict. A transient 404 for a just-created
// parent is retried like the upload-session create it replaces.
func (c *Client) CreateFile(ctx context.Context, driveID driveid.ID, parentID, name string) (*Item, error) {
	c.logger.Debug("creating file",
		slog.String("drive_id", driveID.String()),
		slog.String("parent_id", parentID),
		slog.String("name", name),
	)

	bodyBytes, err := json.Marshal(createFileRequest{Name: name, ConflictBehavior: "fail"})
	if err != nil {
		return nil, fmt.Errorf("graph: marshaling create file request: %w", err)
	}

	path := fmt.Sprintf("/drives/%s/items/%s/children", driveID, parentID)

	resp, err := doDocumentedGraphQuirkRetry(ctx, c, documentedGraphQuirkSpec{
		name:   QuirkUploadSessionCreateTransient404,
		policy: c.policies.uploadSessionCreate,
		match:  isTransientUploadSessionCreateError,
	}, func() (*http.Response, error) {
		return c.do(ctx, http.MethodPost, path, bytes.NewReader(bodyBytes))
	})
	if err != nil {
		return nil, fmt.Errorf("graph: creating file %q: %w", name, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body is read-only; its close reports nothing a caller can act on

	item, hasItem, err := c.decodeItemBody(resp.Body, "create file")
	if err != nil {
		return nil, err
	}
	if hasItem {
		return item, nil
	}

	return c.readCreatedFileByName(ctx, driveID, parentID, name)
}

// readCreatedFileByName finds a file the create answered without an item.
func (c *Client) readCreatedFileByName(ctx context.Context, driveID driveid.ID, parentID, name string) (*Item, error) {
	children, err := c.ListChildren(ctx, driveID, parentID)
	if err != nil {
		return nil, fmt.Errorf("graph: reading back created file %q: %w", name, err)
	}

	for i := range children {
		if !children[i].IsFolder && children[i].ID != "" && namekey.Equal(children[i].Name, name) {
			return &children[i], nil
		}
	}

	return nil, fmt.Errorf("graph: created file %q is not in its folder: %w", name, ErrUploadCompletedWithoutItem)
}

// Upload creates a new file under parentID and uploads its content (see the
// comment at the top of this file). A zero-byte file is the created item
// plus an mtime PATCH; any other size is sent through an upload session on
// the new item, conditional on the eTag it was created with, so a writer that
// replaced it in between gets a 412 instead of being overwritten. When the
// content step fails before its final fragment the empty item is removed
// again (conditionally), so a retry does not meet its own placeholder as a
// conflicting file. content must be an io.ReaderAt so that retries can
// re-read from arbitrary offsets. progress may be nil.
//
// A zero-byte upload whose content landed but whose mtime PATCH failed
// returns the uploaded item together with a *MtimeNotAppliedError.
func (c *Client) Upload(
	ctx context.Context, driveID driveid.ID, parentID, name string,
	content io.ReaderAt, size int64, mtime time.Time, progress ProgressFunc,
) (*Item, error) {
	created, err := c.CreateFile(ctx, driveID, parentID, name)
	if err != nil {
		return nil, err
	}

	if size == 0 {
		return c.finalizeSimpleUpload(ctx, driveID, created, mtime)
	}

	item, err := c.chunkedUploadToExistingItem(ctx, driveID, created.ID, created.ETag, content, size, mtime, progress)
	if err != nil {
		c.removeCreatedPlaceholder(ctx, driveID, created, err)
		return nil, err
	}

	return item, nil
}

// CreateUploadSession creates the file named name under parentID and opens a
// resumable upload session on it, for callers that persist the session.
// Like Upload it never replaces an existing name, and the session is
// conditional on the created item's eTag.
func (c *Client) CreateUploadSession(
	ctx context.Context, driveID driveid.ID, parentID, name string, size int64, mtime time.Time,
) (*UploadSession, error) {
	created, err := c.CreateFile(ctx, driveID, parentID, name)
	if err != nil {
		return nil, err
	}

	session, err := c.CreateUploadSessionForItem(ctx, driveID, created.ID, created.ETag, size, mtime)
	if err != nil {
		c.removeCreatedPlaceholder(ctx, driveID, created, err)
		return nil, err
	}

	return session, nil
}

// removeCreatedPlaceholder deletes the empty item a failed upload created,
// unless the failure was on the final fragment (the content may have
// landed; the caller looks before deciding). The delete is conditional on
// the creation eTag, so it can only remove the untouched empty item, and is
// best-effort: a leftover placeholder is an ordinary remote file the next
// pass sees.
func (c *Client) removeCreatedPlaceholder(ctx context.Context, driveID driveid.ID, created *Item, cause error) {
	if IsFinalFragmentUncertain(cause) || created.ETag == "" {
		return
	}

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	if err := c.DeleteItemIfMatch(cleanupCtx, driveID, created.ID, created.ETag); err != nil {
		c.logger.Debug("failed to remove the empty file a failed upload created",
			slog.String("item_id", created.ID),
			slog.String("error", err.Error()),
		)
	}
}
