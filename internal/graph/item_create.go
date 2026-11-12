package graph

import (
	"context"
	"encoding/json"
	"errors"
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
// That engine has to know which failures may have left a file behind. A
// CreateFile failure that does not say whether the create landed carries
// ErrCreateOutcomeUnknown: no answer at all (a dropped connection, a context
// that ended while the request was out), a 5xx, a 408, and any failure after
// a 2xx answer that did not yield the item. A definite refusal (409, 429, any
// other 4xx) does not, and neither does a create that was never sent (the
// token source failed, the context had already ended, the request could not
// be built, or the retry after a refusal never left). Sync keeps its record
// of the intended create through the first kind (R-5.6.15), so the empty file
// such a create left is recognized as its own.
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
// parent is retried (QuirkFileCreateTransient404). A failure that leaves open
// whether the file was created wraps ErrCreateOutcomeUnknown; one before the
// create left the client never does.
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
		name:   QuirkFileCreateTransient404,
		policy: c.policies.fileCreate,
		match:  isTransientFileCreateError,
	}, func() (*http.Response, error) {
		return c.do(retry.WithReplaySafe(ctx), http.MethodPost, path, bodyBytes)
	})
	if err != nil {
		if createOutcomeUnknown(err) {
			return nil, fmt.Errorf("graph: creating file %q: %w: %w", name, ErrCreateOutcomeUnknown, err)
		}

		return nil, fmt.Errorf("graph: creating file %q: %w", name, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body is read-only; its close reports nothing a caller can act on

	// From here on OneDrive has answered 2xx: the file exists, and a failure
	// to learn its item leaves the caller without the ID of a file it made.
	item, hasItem, err := c.decodeItemBody(resp.Body, "create file")
	if err != nil {
		return nil, fmt.Errorf("graph: reading the answer to creating file %q: %w: %w", name, ErrCreateOutcomeUnknown, err)
	}
	if hasItem {
		return item, nil
	}

	return c.readCreatedFileByName(ctx, driveID, parentID, name)
}

// createOutcomeUnknown reports whether err, the failure of the request that
// creates a file, leaves open whether the file was created. A request that
// never left (errRequestNotSent) created nothing, and so did every attempt
// the graph client itself made before it in the same call: doOnce sends again
// only after a 401 and the quirk retry only after a 404 itemNotFound, both
// refusals. Any other answer short of a definite refusal leaves it open: no
// answer at all (a transport failure, a context that ended while the request
// was out: nothing that is a *GraphError), a 5xx, or a 408. A 4xx other than
// 408 is a refusal: nothing was created.
//
// Only the answers the graph client sees are judged. A retrying transport
// makes attempts of its own inside one dispatch, and CreateFile marks its
// request replay safe, so under the CLI's transport a 5xx attempt can sit
// unseen before the last refusal or before a retry that was not sent. Sync,
// the only reader of ErrCreateOutcomeUnknown, uses a transport that retries
// nothing (R-6.8.8), so there every attempt is one the client saw.
func createOutcomeUnknown(err error) bool {
	if errors.Is(err, errRequestNotSent) {
		return false
	}

	var graphErr *GraphError
	if !errors.As(err, &graphErr) {
		return true
	}

	return graphErr.StatusCode >= http.StatusInternalServerError || graphErr.StatusCode == http.StatusRequestTimeout
}

// readCreatedFileByName finds a file the create answered without an item.
// The create itself succeeded, so every failure here wraps
// ErrCreateOutcomeUnknown: the file exists and the caller has no ID for it.
// A file the folder does not list yet is that failure too, and never
// ErrUploadCompletedWithoutItem: no content was sent, and the transfer layer
// answers that error by looking the name up and checking the file against the
// content, which fails an empty file as a bad upload and loses the mark.
func (c *Client) readCreatedFileByName(ctx context.Context, driveID driveid.ID, parentID, name string) (*Item, error) {
	children, err := c.ListChildren(ctx, driveID, parentID)
	if err != nil {
		return nil, fmt.Errorf("graph: reading back created file %q: %w: %w", name, ErrCreateOutcomeUnknown, err)
	}

	for i := range children {
		if !children[i].IsFolder && children[i].ID != "" && namekey.Equal(children[i].Name, name) {
			return &children[i], nil
		}
	}

	return nil, fmt.Errorf("graph: created file %q is not in its folder: %w", name, ErrCreateOutcomeUnknown)
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
// item is removed again (RemoveEmptyItem), so a retry does not meet its own
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

// placeholderCleanupTimeout bounds the whole removal of the empty item a
// failed upload created: every read and delete of RemoveEmptyItem together,
// on a context detached from the upload's.
const placeholderCleanupTimeout = 10 * time.Second

// placeholderCleanupAttempts is how many times RemoveEmptyItem reads the item
// and tries its conditional delete before it leaves the item in place.
const placeholderCleanupAttempts = 3

// removeCreatedPlaceholder deletes the empty item a failed upload created,
// unless the failure was on the final fragment (the content may have
// landed; the caller looks before deciding). The removal itself is
// RemoveEmptyItem, on a context detached from the upload's (the upload may be
// failing because that was canceled) and bounded by
// placeholderCleanupTimeout. Best-effort: a leftover placeholder is an
// ordinary remote file the next pass sees.
func (c *Client) removeCreatedPlaceholder(ctx context.Context, driveID driveid.ID, created *Item, cause error) {
	if IsFinalFragmentUncertain(cause) || created == nil || created.ID == "" {
		return
	}

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), placeholderCleanupTimeout)
	defer cancel()

	removed, err := c.RemoveEmptyItem(cleanupCtx, driveID, created.ID)
	if err != nil {
		c.logger.Debug("failed to remove the empty file a failed upload created",
			slog.String("item_id", created.ID),
			slog.Any("error", err),
		)
		return
	}
	if !removed {
		c.logger.Debug("left the file a failed upload created in place: it holds content, or has no eTag to delete it by",
			slog.String("item_id", created.ID),
		)
	}
}

// RemoveEmptyItem deletes itemID while it is still an empty file. Each attempt
// reads the item and deletes it conditional on the eTag just read; a 412 is
// answered by reading again, at most placeholderCleanupAttempts times.
// removed is true when the item is gone (deleted here, or already 404). An
// item that holds content, or that reads back without an eTag, is never
// deleted: removed is false and err is nil.
//
// One read and one delete are not enough. An upload session opened on the
// empty item moves its eTag although no content landed (live, e2e
// TestE2E_SyncWatch_FileModification), so the creation eTag never matches;
// and the eTag can still move after the read, which the delete then answers
// with 412 and which left the empty file behind (live, e2e
// TestE2E_SyncWatch_PauseResume). The caller bounds the whole call through
// ctx.
func (c *Client) RemoveEmptyItem(ctx context.Context, driveID driveid.ID, itemID string) (removed bool, err error) {
	var refused error

	for range placeholderCleanupAttempts {
		current, readErr := c.GetItem(ctx, driveID, itemID)
		if errors.Is(readErr, ErrNotFound) {
			return true, nil
		}
		if readErr != nil {
			return false, fmt.Errorf("graph: reading empty item %s: %w", itemID, readErr)
		}
		if current.Size != 0 || current.ETag == "" {
			return false, nil
		}

		deleteErr := c.DeleteItemIfMatch(ctx, driveID, itemID, current.ETag)
		if deleteErr == nil || errors.Is(deleteErr, ErrNotFound) {
			return true, nil
		}
		if !errors.Is(deleteErr, ErrPreconditionFailed) {
			return false, fmt.Errorf("graph: deleting empty item %s: %w", itemID, deleteErr)
		}

		// The eTag moved between the read and the delete: read again.
		refused = deleteErr
	}

	return false, fmt.Errorf("graph: removing empty item %s: %w", itemID, refused)
}
