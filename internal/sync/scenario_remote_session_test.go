package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Resumable upload sessions in the scenario fake. driveops sends a file above
// 4 MiB through a recorded session when its uploader offers one
// (driveops.SessionUploader, ItemSessionUploader), and engineMockClient does
// not: without these, scenario, chaos and model tests never ran the large-file
// route, where the session request of a new file fails after its create.

// scenarioSession is one open upload session: the item it writes and that
// item's eTag when it was opened.
type scenarioSession struct {
	itemID string
	etag   string
}

// scenarioSessionExpiry is when every scenario session expires: far enough
// out that a session the remote no longer holds reads as invalidated by a
// change, as on OneDrive, never as expired.
func scenarioSessionExpiry() time.Time {
	return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
}

// scenarioSessionClient is the scenario fake's client with Graph's upload
// sessions. It is its own type because a method set cannot be optional: on
// engineMockClient itself, every engine test with a large file would change
// route. Tests opt in with newScenarioSessionEngine.
type scenarioSessionClient struct {
	*engineMockClient
	t      *testing.T
	remote *moveScenarioRemote
}

var (
	_ driveops.SessionUploader     = (*scenarioSessionClient)(nil)
	_ driveops.ItemSessionUploader = (*scenarioSessionClient)(nil)
	_ driveops.SessionCanceler     = (*scenarioSessionClient)(nil)
)

func (r *moveScenarioRemote) sessionClient(t *testing.T) *scenarioSessionClient {
	t.Helper()

	return &scenarioSessionClient{engineMockClient: r.client(t), t: t, remote: r}
}

// newScenarioSessionEngine is a test engine whose uploads go through client,
// so a file above 4 MiB takes the session route.
func newScenarioSessionEngine(t *testing.T, client *scenarioSessionClient) (*testEngine, string) {
	t.Helper()

	return newTestEngineInDirWith(t, t.Context(), client.engineMockClient, t.TempDir(), func(inputs *engineInputs) {
		inputs.Uploads = client
	})
}

// CreateUploadSession is graph.Client's one call for a large new file: the
// empty item by body with conflictBehavior=fail, then a session on it.
func (c *scenarioSessionClient) CreateUploadSession(
	_ context.Context, _ driveid.ID, parentID, name string, _ int64, _ time.Time,
) (*graph.UploadSession, error) {
	return c.remote.createUploadSession(c.t, parentID, name)
}

// CreateUploadSessionForItem opens a session on an existing item.
func (c *scenarioSessionClient) CreateUploadSessionForItem(
	_ context.Context, _ driveid.ID, itemID, ifMatch string, _ int64, _ time.Time,
) (*graph.UploadSession, error) {
	return c.remote.createUploadSessionForItem(itemID, ifMatch)
}

// UploadFromSession sends the content through a fresh session.
func (c *scenarioSessionClient) UploadFromSession(
	_ context.Context, session *graph.UploadSession, content io.ReaderAt, totalSize int64, _ graph.ProgressFunc,
) (*graph.Item, error) {
	return c.remote.uploadThroughSession(c.t, session.UploadURL, content, totalSize, graph.ErrNotFound)
}

// ResumeUpload resumes a recorded session; one OneDrive no longer holds has
// expired.
func (c *scenarioSessionClient) ResumeUpload(
	_ context.Context, session *graph.UploadSession, content io.ReaderAt, totalSize int64, _ graph.ProgressFunc,
) (*graph.Item, error) {
	return c.remote.uploadThroughSession(c.t, session.UploadURL, content, totalSize, graph.ErrUploadSessionExpired)
}

// CancelUploadSession drops a session.
func (c *scenarioSessionClient) CancelUploadSession(_ context.Context, session *graph.UploadSession) error {
	c.remote.mu.Lock()
	defer c.remote.mu.Unlock()
	delete(c.remote.sessions, session.UploadURL)

	return nil
}

// createUploadSession creates the empty item and opens a session on it. The
// knobs of the small-file create apply: refuseCreate refuses before anything
// is made, loseCreateAnswers loses the create's answer, and interruptCreates
// fails the session request once the item exists and leaves the item there,
// as a session request the client gave up on whose cleanup was then refused
// did (LI-20261110-01); churnInterruptedCreates also moves its eTag.
func (r *moveScenarioRemote) createUploadSession(t *testing.T, parentID, name string) (*graph.UploadSession, error) {
	t.Helper()
	if refusal := r.refusal(parentID, name); refusal != nil {
		return nil, refusal
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.childNamed(parentID, name); exists {
		return nil, scenarioNameConflict()
	}
	r.uploads++
	id := r.nextID("created")
	r.addFile(t, id, parentID, name, "")
	created := *r.items[id]

	switch {
	case r.loseCreateAnswers:
		return nil, scenarioCreateAnswerLost(name)
	case r.interruptCreates:
		if r.churnInterruptedCreates {
			r.churnETagLocked(id)
		}
		return nil, &graph.CreatedItemError{Item: &created, Err: errors.New(scenarioConnectionReset)}
	}

	session := r.openSessionLocked(id)
	session.CreatedETag = created.ETag

	return session, nil
}

// createUploadSessionForItem opens a session on an existing item, honoring
// If-Match like Graph does (live probe L-02).
func (r *moveScenarioRemote) createUploadSessionForItem(itemID, ifMatch string) (*graph.UploadSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.items[itemID]
	if !ok {
		return nil, graph.ErrNotFound
	}
	if ifMatch != "" && item.ETag != ifMatch {
		return nil, scenarioPreconditionFailed()
	}

	return r.openSessionLocked(itemID), nil
}

func (r *moveScenarioRemote) openSessionLocked(itemID string) *graph.UploadSession {
	r.sessionSeq++
	url := graph.UploadURL(fmt.Sprintf("https://upload.scenario.invalid/session-%d", r.sessionSeq))
	if r.sessions == nil {
		r.sessions = map[graph.UploadURL]scenarioSession{}
	}
	r.sessions[url] = scenarioSession{itemID: itemID, etag: r.items[itemID].ETag}

	return &graph.UploadSession{UploadURL: url, ExpirationTime: scenarioSessionExpiry(), ItemID: itemID}
}

// uploadThroughSession sends the whole content through the session at url
// and commits it to the session's item. A session OneDrive no longer holds
// answers gone; one whose item changed since it opened was invalidated by
// that change and answers 404 (live probe L-02).
func (r *moveScenarioRemote) uploadThroughSession(
	t *testing.T, url graph.UploadURL, content io.ReaderAt, size int64, gone error,
) (*graph.Item, error) {
	t.Helper()
	data, err := readScenarioUpload(content, size)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[url]
	if !ok {
		return nil, gone
	}
	delete(r.sessions, url)
	item, ok := r.items[session.itemID]
	if !ok || item.ETag != session.etag {
		return nil, graph.ErrNotFound
	}
	r.uploads++
	r.setContent(t, session.itemID, data)
	copied := *r.items[session.itemID]

	return &copied, nil
}
