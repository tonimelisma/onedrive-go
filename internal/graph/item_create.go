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
	"github.com/tonimelisma/onedrive-go/internal/retry"
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
// fail is also what makes the create safe to resend after a transient
// failure that does not say whether it committed (a 503, a dropped
// connection): a replay of a create that landed answers 409 and creates
// nothing, and every caller already takes a 409 as "look at what holds the
// name". CreateFile therefore marks its request retry.WithReplaySafe, and a
// retrying (CLI) transport resends it; sync's transport retries nothing
// (R-6.8.8), so there a transient answer still goes to the engine.
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
		return c.do(retry.WithReplaySafe(ctx), http.MethodPost, path, bytes.NewReader(bodyBytes))
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

// CreatedFunc is called after a new file's empty item exists and before any
// content is sent. Sync records the pending create durably there, so an
// interrupted upload's placeholder is recognized as its own (R-5.6.15). An
// error aborts the upload and removes the placeholder.
type CreatedFunc func(created *Item) error

// CreatedItemError reports a failure after the new file's item was created.
// Item is that item, so callers look the outcome up by its ID rather than by
// name.
type CreatedItemError struct {
	Item *Item
	Err  error
}

func (e *CreatedItemError) Error() string { return e.Err.Error() }

func (e *CreatedItemError) Unwrap() error { return e.Err }

// Upload creates a new file under parentID and uploads its content (see the
// comment at the top of this file). It is UploadCreating without a hook.
func (c *Client) Upload(
	ctx context.Context, driveID driveid.ID, parentID, name string,
	content io.ReaderAt, size int64, mtime time.Time, progress ProgressFunc,
) (*Item, error) {
	return c.UploadCreating(ctx, driveID, parentID, name, content, size, mtime, progress, nil)
}

// UploadCreating creates a new file under parentID and uploads its content.
// A zero-byte file is the created item plus an mtime PATCH; any other size is
// sent through an upload session on the new item, conditional on the eTag it
// was created with, so a writer that replaced it in between gets a 412
// instead of being overwritten. onCreated (may be nil) runs between the two
// steps. When the content step fails before its final fragment the empty
// item is removed again (conditionally), so a retry does not meet its own
// placeholder as a conflicting file; failures after the create are
// *CreatedItemError. content must be an io.ReaderAt so that retries can
// re-read from arbitrary offsets. progress may be nil.
//
// A zero-byte upload whose content landed but whose mtime PATCH failed
// returns the uploaded item together with a *MtimeNotAppliedError.
func (c *Client) UploadCreating(
	ctx context.Context, driveID driveid.ID, parentID, name string,
	content io.ReaderAt, size int64, mtime time.Time, progress ProgressFunc, onCreated CreatedFunc,
) (*Item, error) {
	created, err := c.CreateFile(ctx, driveID, parentID, name)
	if err != nil {
		return nil, err
	}

	if size == 0 {
		return c.finalizeSimpleUpload(ctx, driveID, created, mtime)
	}

	if onCreated != nil {
		if hookErr := onCreated(created); hookErr != nil {
			c.removeCreatedPlaceholder(ctx, driveID, created, hookErr)
			return nil, &CreatedItemError{Item: created, Err: hookErr}
		}
	}

	item, err := c.chunkedUploadToExistingItem(ctx, driveID, created.ID, created.ETag, content, size, mtime, progress)
	if err != nil {
		c.removeCreatedPlaceholder(ctx, driveID, created, err)
		return nil, &CreatedItemError{Item: created, Err: err}
	}

	return item, nil
}

// CreateUploadSession creates the file named name under parentID and opens a
// resumable upload session on it, for callers that persist the session.
// Like Upload it never replaces an existing name, and the session is
// conditional on the created item's eTag; the session names the created item
// (ItemID, CreatedETag) so the caller can record it before sending content.
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
		return nil, &CreatedItemError{Item: created, Err: err}
	}
	session.CreatedETag = created.ETag

	return session, nil
}

// removeCreatedPlaceholder deletes the empty item a failed upload created,
// unless the failure was on the final fragment (the content may have
// landed; the caller looks before deciding). It reads the item again and
// deletes it only while it is still empty, conditional on the eTag just
// read: an upload session opened on the empty item moves its eTag although
// no content landed (live, e2e TestE2E_SyncWatch_FileModification), so a
// delete conditional on the creation eTag failed with 412 and left the
// placeholder. Best-effort: a leftover placeholder is an ordinary remote file
// the next pass sees.
func (c *Client) removeCreatedPlaceholder(ctx context.Context, driveID driveid.ID, created *Item, cause error) {
	if IsFinalFragmentUncertain(cause) || created == nil || created.ID == "" {
		return
	}

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	current, err := c.GetItem(cleanupCtx, driveID, created.ID)
	if err != nil {
		c.logger.Debug("failed to read the empty file a failed upload created",
			slog.String("item_id", created.ID),
			slog.Any("error", err),
		)
		return
	}
	if current.Size != 0 || current.ETag == "" {
		return
	}

	if err := c.DeleteItemIfMatch(cleanupCtx, driveID, created.ID, current.ETag); err != nil {
		c.logger.Debug("failed to remove the empty file a failed upload created",
			slog.String("item_id", created.ID),
			slog.Any("error", err),
		)
	}
}
