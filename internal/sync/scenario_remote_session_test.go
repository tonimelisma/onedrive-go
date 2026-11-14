package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Resumable upload sessions in the scenario fake. driveops sends a file above
// 4 MiB through a recorded session when its uploader offers one
// (driveops.SessionUploader, ItemSessionUploader), and engineMockClient does
// not: without these, scenario, chaos and model tests never ran the large-file
// route, where the session request of a new file fails after its create.
// Content goes up in fragments and a session keeps what it accepted, so a
// fault between two fragments leaves a part-uploaded session for a resume to
// finish.

// scenarioSession is one open upload session: the item it writes, that
// item's eTag when it was opened, and the content it has accepted so far,
// from offset 0. A request that fails loses none of it: OneDrive keeps a
// session's fragments and answers a status query with the next expected
// range.
type scenarioSession struct {
	itemID   string
	etag     string
	received string
}

// scenarioSessionFragmentSize is the size of every fragment but the last the
// fake's session client sends. Graph's client sends 10 MiB
// (graph.ChunkedUploadChunkSize); the fake sends an eighth of that, which is
// four times Graph's 320 KiB alignment, so a file just above the 4 MiB
// session threshold spans several fragments and a fault can land between two
// of them.
const scenarioSessionFragmentSize = graph.ChunkedUploadChunkSize / 8

// scenarioFragment is one fragment that reached a session: where it starts,
// how long it is, and whether the session accepted it (it was the next
// expected range).
type scenarioFragment struct {
	offset   int64
	size     int64
	accepted bool
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
	// chaos, when set, is the chaos Graph layer between the client and the
	// fake's sessions (chaosGraph.wrapSessions).
	chaos *chaosGraph
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
	create := func() (*graph.UploadSession, error) { return c.remote.createUploadSession(c.t, parentID, name) }
	if c.chaos == nil {
		return create()
	}

	return c.chaos.createUploadSession(c.remote, parentID, name, create)
}

// CreateUploadSessionForItem opens a session on an existing item.
func (c *scenarioSessionClient) CreateUploadSessionForItem(
	_ context.Context, _ driveid.ID, itemID, ifMatch string, _ int64, _ time.Time,
) (*graph.UploadSession, error) {
	open := func() (*graph.UploadSession, error) { return c.remote.createUploadSessionForItem(itemID, ifMatch) }
	if c.chaos == nil {
		return open()
	}

	return c.chaos.createUploadSessionForItem(itemID, open)
}

// UploadFromSession sends the content through a fresh session, fragment by
// fragment, as graph.Client does.
func (c *scenarioSessionClient) UploadFromSession(
	_ context.Context, session *graph.UploadSession, content io.ReaderAt, totalSize int64, _ graph.ProgressFunc,
) (*graph.Item, error) {
	return c.sendFragments(c.faultStream(chaosOpSessionUpload, session), chaosOpSessionUpload, session, content, totalSize, 0)
}

// ResumeUpload asks the session for its next expected range and sends the
// content from there, as graph.Client does; a session OneDrive no longer
// holds has expired.
func (c *scenarioSessionClient) ResumeUpload(
	_ context.Context, session *graph.UploadSession, content io.ReaderAt, totalSize int64, _ graph.ProgressFunc,
) (*graph.Item, error) {
	cc := c.faultStream(chaosOpSessionResume, session)
	if cc != nil {
		if err := c.chaos.before(cc, chaosOpSessionResume); err != nil {
			return nil, fmt.Errorf("graph: querying session for resume: %w", err)
		}
	}
	next, open := c.remote.sessionNextExpected(session.UploadURL)
	if !open {
		return nil, fmt.Errorf("graph: querying session for resume: %w", graph.ErrUploadSessionExpired)
	}

	return c.sendFragments(cc, chaosOpSessionResume, session, content, totalSize, next)
}

// faultStream is the chaos layer's fault stream for one upload call through
// session, or nil without the layer.
func (c *scenarioSessionClient) faultStream(op string, session *graph.UploadSession) *chaosCall {
	if c.chaos == nil {
		return nil
	}

	return c.chaos.call(op, chaosSessionKey(session))
}

// sendFragments sends content[from:] through the session in fragments of
// scenarioSessionFragmentSize and returns the item the last one commits. A
// failure stops it: what the session accepted before stays there for a
// resume. As in graph.Client, a failure of the final fragment is uncertain
// (graph.FinalFragmentError), because the content may have been committed.
func (c *scenarioSessionClient) sendFragments(
	cc *chaosCall, op string, session *graph.UploadSession, content io.ReaderAt, totalSize, from int64,
) (*graph.Item, error) {
	for offset := from; offset < totalSize; {
		size := min(int64(scenarioSessionFragmentSize), totalSize-offset)
		item, complete, err := c.sendFragment(cc, op, session, content, offset, size, totalSize)
		switch {
		case err != nil && offset+size == totalSize:
			return nil, fmt.Errorf("graph: uploading final chunk at offset %d: %w", offset, &graph.FinalFragmentError{Err: err})
		case err != nil:
			return nil, fmt.Errorf("graph: uploading chunk at offset %d: %w", offset, err)
		case complete:
			return item, nil
		}
		offset += size
	}

	return nil, fmt.Errorf("graph: upload session has no range left to send from offset %d of %d", from, totalSize)
}

// sendFragment sends one fragment through the chaos layer, when there is
// one: it may fail before it lands, and the final fragment may land and
// lose its answer. It reports whether the fragment completed the content.
func (c *scenarioSessionClient) sendFragment(
	cc *chaosCall, op string, session *graph.UploadSession, content io.ReaderAt, offset, size, totalSize int64,
) (*graph.Item, bool, error) {
	data := make([]byte, size)
	if _, err := content.ReadAt(data, offset); err != nil && err != io.EOF {
		return nil, false, fmt.Errorf("read upload content at offset %d: %w", offset, err)
	}
	if cc != nil {
		if err := c.chaos.fragmentFault(cc, op, offset); err != nil {
			return nil, false, err
		}
	}
	item, complete, err := c.remote.putFragment(c.t, session.UploadURL, offset, string(data), totalSize)
	if err != nil || !complete || cc == nil || !cc.roll(chaosFaultOdds) {
		return item, complete, err
	}
	_, lostErr := c.chaos.lostResponseError(cc, op)

	return nil, true, lostErr
}

// CancelUploadSession drops a session.
func (c *scenarioSessionClient) CancelUploadSession(_ context.Context, session *graph.UploadSession) error {
	cancel := func() error {
		c.remote.dropSession(session.UploadURL)
		return nil
	}
	if c.chaos == nil {
		return cancel()
	}

	return c.chaos.cancelSession(session, cancel)
}

// dropSession forgets the session at url, open or not.
func (r *moveScenarioRemote) dropSession(url graph.UploadURL) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, url)
}

// holdsSession reports whether the session at url is open.
func (r *moveScenarioRemote) holdsSession(url graph.UploadURL) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.sessions[url]

	return ok
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

// sessionNextExpected is the start of the next expected range of the session
// at url, as its status answers; open is false for a session OneDrive no
// longer holds.
func (r *moveScenarioRemote) sessionNextExpected(url graph.UploadURL) (next int64, open bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[url]

	return int64(len(session.received)), ok
}

// putFragment accepts data at offset into the session at url, as Graph's
// fragment PUT does, and reports whether the fragment completed the content,
// with the item it then committed. A session OneDrive no longer holds, and
// one whose item changed since it opened (invalidated by that change, live
// probe L-02), answer 404; a fragment that is not the next expected range
// answers 416 and is not accepted. The fragment that completes the content
// commits it to the item and closes the session. interruptFragmentAt drops a
// fragment before it reaches the session.
func (r *moveScenarioRemote) putFragment(
	t *testing.T, url graph.UploadURL, offset int64, data string, totalSize int64,
) (*graph.Item, bool, error) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.interruptFragmentAt != 0 && offset == r.interruptFragmentAt {
		r.interruptFragmentAt = 0
		return nil, false, errors.New(scenarioConnectionReset)
	}
	session, ok := r.sessions[url]
	if !ok {
		return nil, false, graph.ErrNotFound
	}
	item, ok := r.items[session.itemID]
	if !ok || item.ETag != session.etag {
		delete(r.sessions, url)
		return nil, false, graph.ErrNotFound
	}
	if r.sessionFragments == nil {
		r.sessionFragments = map[graph.UploadURL][]scenarioFragment{}
	}
	fragment := scenarioFragment{offset: offset, size: int64(len(data)), accepted: offset == int64(len(session.received))}
	r.sessionFragments[url] = append(r.sessionFragments[url], fragment)
	if !fragment.accepted {
		return nil, false, &graph.GraphError{StatusCode: http.StatusRequestedRangeNotSatisfiable, Err: graph.ErrRangeNotSatisfiable}
	}
	session.received += data
	if int64(len(session.received)) < totalSize {
		r.sessions[url] = session
		return nil, false, nil
	}

	delete(r.sessions, url)
	r.uploads++
	r.setContent(t, session.itemID, session.received)
	copied := *r.items[session.itemID]

	return &copied, true, nil
}

// fragmentsReceived lists the fragments that reached the session at url, in
// the order they arrived.
func (r *moveScenarioRemote) fragmentsReceived(url graph.UploadURL) []scenarioFragment {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]scenarioFragment(nil), r.sessionFragments[url]...)
}

// Validates: R-5.2.4
//
// A session keeps the fragments it accepted across a failed request, and a
// resume asks for the next expected range and sends only what is missing:
// every fragment that reaches the session is the next expected range, and
// together they are the file, once.
func TestScenarioSession_ResumeSendsOnlyTheMissingRanges(t *testing.T) {
	t.Parallel()

	const (
		fragmentsInTheFile     = 4
		acceptedBeforeTheFault = 2
		lastFragmentSize       = 1024
	)
	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	client := remote.sessionClient(t)
	content := strings.Repeat("r", (fragmentsInTheFile-1)*scenarioSessionFragmentSize+lastFragmentSize)
	size := int64(len(content))
	session, err := client.CreateUploadSession(t.Context(), remote.driveID, graphRootID, "big.bin", size, time.Time{})
	require.NoError(t, err)

	remote.mu.Lock()
	remote.interruptFragmentAt = acceptedBeforeTheFault * scenarioSessionFragmentSize
	remote.mu.Unlock()
	_, err = client.UploadFromSession(t.Context(), session, strings.NewReader(content), size, nil)
	require.Error(t, err)
	assert.False(t, graph.IsFinalFragmentUncertain(err), "a fragment before the last failed: %v", err)
	next, open := remote.sessionNextExpected(session.UploadURL)
	require.True(t, open, "the session survives the failed fragment")
	assert.Equal(t, int64(acceptedBeforeTheFault*scenarioSessionFragmentSize), next, "the session holds the fragments it accepted")

	// driveops resumes from the recorded URL alone.
	item, err := client.ResumeUpload(t.Context(), &graph.UploadSession{UploadURL: session.UploadURL}, strings.NewReader(content), size, nil)
	require.NoError(t, err)
	assert.Equal(t, size, item.Size)
	assert.Equal(t, content, remote.tree()["big.bin"])
	assert.False(t, remote.holdsSession(session.UploadURL), "the last fragment closed the session")

	var sent int64
	fragments := remote.fragmentsReceived(session.UploadURL)
	for i, fragment := range fragments {
		assert.True(t, fragment.accepted, "fragment %d at %d resent accepted bytes", i, fragment.offset)
		assert.Equal(t, sent, fragment.offset, "fragment %d starts where the session's content ended", i)
		sent += fragment.size
	}
	assert.Equal(t, size, sent, "every byte reached the session once")
	assert.Len(t, fragments, fragmentsInTheFile, "the fragments before the fault, then the rest")
}
