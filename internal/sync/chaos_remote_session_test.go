package sync

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// The chaos Graph layer on the session route (D-092). A file above 4 MiB goes
// through Graph's upload sessions (scenarioSessionClient), and in a storm
// those calls fail the ways the small-file route's do: before the request
// reaches OneDrive, or after it took effect with its answer lost. The large
// file's create is one Graph call for two requests (graph.CreateUploadSession:
// the empty item, then a session on it), so it also fails between them: the
// session request times out once the empty item exists, some of them after
// that item's eTag moved. Content goes up in fragments, each of which may
// fail on its own, so a fault can leave a part-uploaded session.

const (
	chaosOpCreateSession = "create-session"
	chaosOpItemSession   = "item-session"
	chaosOpSessionUpload = "session-upload"
	chaosOpSessionResume = "session-resume"
	chaosOpSessionCancel = "session-cancel"

	// The fault kinds of a large file's create that took effect: its own
	// answer lost, or its session request timed out once the empty item
	// existed, and of those the ones whose item's eTag moved meanwhile.
	chaosCreateSessionAnswerLost = chaosOpCreateSession + chaosResponseLostSuffix
	chaosCreateSessionTimedOut   = chaosOpCreateSession + ":timeout-after-create"
	chaosCreateSessionETagMoved  = chaosCreateSessionTimedOut + ":etag-moved"
	// chaosSessionUploadAnswerLost is a session upload whose content landed
	// and whose answer was lost.
	chaosSessionUploadAnswerLost = chaosOpSessionUpload + chaosResponseLostSuffix
	// chaosPartSentSuffix ends the fault kind of a fragment that failed after
	// the session had accepted earlier ones: the session holds part of the
	// file.
	chaosPartSentSuffix = ":part-sent"
)

// wrapSessions returns a session client whose every call passes through the
// chaos layer: the small-file calls as wrap's do, and the session route's.
func (c *chaosGraph) wrapSessions(inner *scenarioSessionClient) *scenarioSessionClient {
	out := *inner
	out.engineMockClient = c.wrap(inner.engineMockClient)
	out.chaos = c

	return &out
}

// createUploadSession runs a large file's create through the layer. It may
// fail before reaching OneDrive (as graph.CreateFile reports it), lose the
// create's own answer, or time out on the session request once the empty
// item exists. Either fault after the create leaves no session open, and,
// as with the small-file create, the empty item stays: Graph's cleanup of it
// is best effort and was refused live (LI-20261110-01).
func (c *chaosGraph) createUploadSession(
	remote *moveScenarioRemote, parentID, name string, create func() (*graph.UploadSession, error),
) (*graph.UploadSession, error) {
	cc := c.call(chaosOpCreateSession, parentID+"/"+name)
	if err := c.beforeCreate(cc, chaosOpCreateSession, name); err != nil {
		return nil, err
	}
	session, err := create()
	if err != nil || !cc.roll(chaosFaultOdds) {
		return session, err
	}
	remote.dropSession(session.UploadURL)
	if cc.intN(2) == 0 {
		answer, lostErr := c.lostResponseError(cc, chaosOpCreateSession)
		return nil, chaosCreateFailure(name, answer, lostErr)
	}

	c.count(chaosCreateSessionTimedOut)
	created, err := remote.getItem(session.ItemID)
	if err != nil {
		return nil, err
	}
	if c.churnPlaceholder != nil && cc.intN(2) == 0 {
		c.count(chaosCreateSessionETagMoved)
		c.churnPlaceholder(session.ItemID)
	}

	return nil, &graph.CreatedItemError{
		Item: created,
		Err:  &graph.GraphError{StatusCode: http.StatusGatewayTimeout, Err: graph.ErrServerError},
	}
}

// createUploadSessionForItem may fail before the request reaches OneDrive, or
// open the session and lose the answer: the session exists, unknown to the
// client, as an abandoned session does live until it expires.
func (c *chaosGraph) createUploadSessionForItem(
	itemID string, open func() (*graph.UploadSession, error),
) (*graph.UploadSession, error) {
	return chaosMutation(c, chaosOpItemSession, itemID, open)
}

// fragmentFault is the fault one fragment of a session upload gets before it
// lands, if any. The session keeps what it accepted before, so a fault at a
// nonzero offset leaves a part-uploaded session that a resume finishes by
// sending only the rest (chaosPartSentSuffix). The final fragment may also
// land and lose its answer (scenarioSessionClient.sendFragment), which graph
// reports as uncertain (graph.FinalFragmentError).
func (c *chaosGraph) fragmentFault(cc *chaosCall, op string, offset int64) error {
	err := c.before(cc, op)
	if err != nil && offset > 0 {
		c.count(op + chaosPartSentSuffix)
	}

	return err
}

// cancelSession may fail before the cancel reaches OneDrive, leaving the
// session open.
func (c *chaosGraph) cancelSession(session *graph.UploadSession, cancel func() error) error {
	if err := c.before(c.call(chaosOpSessionCancel, chaosSessionKey(session)), chaosOpSessionCancel); err != nil {
		return err
	}

	return cancel()
}

// chaosSessionKey keys a session's fault stream by the item it writes, as the
// small-file overwrite is keyed.
func chaosSessionKey(session *graph.UploadSession) string {
	if session.ItemID != "" {
		return session.ItemID
	}

	return string(session.UploadURL)
}

// faultsOf counts the faults of one operation, of every kind.
func (c *chaosGraph) faultsOf(op string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for kind, n := range c.faults {
		if strings.HasPrefix(kind, op+":") {
			total += n
		}
	}

	return total
}

const (
	chaosSessionAttempts = 200
	chaosSessionContent  = "large in spirit\n"
)

// chaosSessionOutcomes counts what the storm did to the session route, as the
// test saw it, to compare with the faults the layer counted.
type chaosSessionOutcomes struct {
	createAnswerLost int
	createTimedOut   int
	createETagMoved  int
	uploadAnswerLost int
	landed           []string // item IDs whose content is on OneDrive
	unfinished       []*graph.UploadSession
}

// Validates: R-5.2.1, R-5.6.15
//
// The session route a file above 4 MiB takes passes through the chaos layer,
// and each fault it draws is the failure it models. A create whose answer was
// lost leaves an empty item the client never learned of; one whose session
// request timed out names its empty item, some of them under a moved eTag; one
// that never left the client made nothing. A session upload whose answer was
// lost has landed and reads as uncertain; one that failed before landing
// leaves its session open, and resuming it once the storm passes finishes the
// file. Item sessions and cancels draw faults too.
func TestChaosGraph_SessionRouteDrawsFaults(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	c := newChaosGraph(1)
	c.churnPlaceholder = remote.churnETag
	c.setStorm(true)
	client := c.wrapSessions(remote.sessionClient(t))

	var seen chaosSessionOutcomes
	for i := range chaosSessionAttempts {
		name := fmt.Sprintf("big-%d.bin", i)
		session, err := client.CreateUploadSession(t.Context(), remote.driveID, graphRootID, name, int64(len(chaosSessionContent)), time.Time{})
		if err != nil {
			seen.createFailed(t, remote, name, err)
			continue
		}
		seen.upload(t, client, remote, name, session)
	}
	for _, itemID := range seen.landed {
		requireItemSessionAndCancelHoldUp(t, client, remote, itemID)
	}

	c.setStorm(false)
	for _, session := range seen.unfinished {
		item, err := client.ResumeUpload(t.Context(), session, strings.NewReader(chaosSessionContent), int64(len(chaosSessionContent)), nil)
		require.NoError(t, err, "a session left open resumes once the storm passes")
		assert.Equal(t, chaosSessionContent, remote.tree()[item.Name])
	}

	for what, n := range map[string]int{
		"create answers lost": seen.createAnswerLost, "session requests timed out": seen.createTimedOut,
		"placeholder eTags moved": seen.createETagMoved, "upload answers lost": seen.uploadAnswerLost,
		"uploads left to resume": len(seen.unfinished), "item-session faults": c.faultsOf(chaosOpItemSession),
		"cancel faults": c.faultsOf(chaosOpSessionCancel),
	} {
		assert.Positive(t, n, "no %s in %d attempts", what, chaosSessionAttempts)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	assert.Equal(t, seen.createAnswerLost, c.faults[chaosCreateSessionAnswerLost])
	assert.Equal(t, seen.createTimedOut, c.faults[chaosCreateSessionTimedOut])
	assert.Equal(t, seen.createETagMoved, c.faults[chaosCreateSessionETagMoved])
	assert.Equal(t, seen.uploadAnswerLost, c.faults[chaosSessionUploadAnswerLost])
}

// createFailed checks what a failed large-file create left on OneDrive.
func (s *chaosSessionOutcomes) createFailed(t *testing.T, remote *moveScenarioRemote, name string, err error) {
	t.Helper()

	item, lookupErr := remote.getItemByPath(name)
	landed := lookupErr == nil
	var created *graph.CreatedItemError
	switch {
	case errors.As(err, &created):
		s.createTimedOut++
		require.True(t, landed, "%s: the empty item a timed-out session request names exists", name)
		assert.Equal(t, created.Item.ID, item.ID, "%s: the failure names the empty item", name)
		assert.Zero(t, item.Size, "%s: no content landed", name)
		if item.ETag != created.Item.ETag {
			s.createETagMoved++
		}
	case errors.Is(err, graph.ErrCreateOutcomeUnknown):
		if landed {
			s.createAnswerLost++
			assert.Zero(t, item.Size, "%s: a create alone carries no content", name)
		}
	default:
		assert.False(t, landed, "%s: a create refused before it reached OneDrive made nothing: %v", name, err)
	}
}

// upload sends the content through session and checks what the storm left.
func (s *chaosSessionOutcomes) upload(
	t *testing.T, client *scenarioSessionClient, remote *moveScenarioRemote, name string, session *graph.UploadSession,
) {
	t.Helper()

	_, err := client.UploadFromSession(t.Context(), session, strings.NewReader(chaosSessionContent), int64(len(chaosSessionContent)), nil)
	switch {
	case err == nil:
		assert.Equal(t, chaosSessionContent, remote.tree()[name], "%s: the upload landed", name)
		s.landed = append(s.landed, session.ItemID)
	case graph.IsFinalFragmentUncertain(err) && remote.tree()[name] == chaosSessionContent:
		s.uploadAnswerLost++
		s.landed = append(s.landed, session.ItemID)
	default:
		// The content is one fragment, so its failure before it landed reads
		// as uncertain too: the client cannot tell the two apart.
		require.True(t, graph.IsFinalFragmentUncertain(err), "%s: a failed final fragment is uncertain: %v", name, err)
		assert.Empty(t, remote.tree()[name], "%s: an upload that failed before it landed sent nothing", name)
		assert.True(t, remote.holdsSession(session.UploadURL), "%s: its session stays open", name)
		s.unfinished = append(s.unfinished, session)
	}
}

// requireItemSessionAndCancelHoldUp opens a session on an existing item and
// cancels it, in the storm: a cancel that failed leaves the session open.
func requireItemSessionAndCancelHoldUp(t *testing.T, client *scenarioSessionClient, remote *moveScenarioRemote, itemID string) {
	t.Helper()

	session, err := client.CreateUploadSessionForItem(t.Context(), remote.driveID, itemID, "", int64(len(chaosSessionContent)), time.Time{})
	if err != nil {
		return
	}
	if err := client.CancelUploadSession(t.Context(), session); err != nil {
		assert.True(t, remote.holdsSession(session.UploadURL), "a cancel that failed leaves the session open")
		return
	}
	assert.False(t, remote.holdsSession(session.UploadURL), "a cancel that succeeded drops the session")
}

// Validates: R-5.2.4
//
// A storm's fault between two fragments of a session upload leaves the
// session holding the fragments it accepted (chaosPartSentSuffix); once the
// storm passes, a resume sends only the rest, and the file lands whole with
// no byte sent twice.
func TestChaosGraph_FragmentFaultLeavesAPartUploadedSessionToResume(t *testing.T) {
	t.Parallel()

	const (
		attempts          = 24
		fragmentsInAFile  = 3
		lastFragmentBytes = 1
	)
	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	c := newChaosGraph(1)
	client := c.wrapSessions(remote.sessionClient(t))
	content := strings.Repeat("p", (fragmentsInAFile-1)*scenarioSessionFragmentSize+lastFragmentBytes)
	size := int64(len(content))

	resumed := 0
	for i := range attempts {
		name := fmt.Sprintf("part-%d.bin", i)
		c.setStorm(true)
		session, err := client.CreateUploadSession(t.Context(), remote.driveID, graphRootID, name, size, time.Time{})
		if err != nil {
			continue
		}
		_, err = client.UploadFromSession(t.Context(), session, strings.NewReader(content), size, nil)
		next, open := remote.sessionNextExpected(session.UploadURL)
		if err == nil || !open || next == 0 {
			continue // landed, or failed before the session held a byte
		}
		resumed++
		c.setStorm(false)
		_, err = client.ResumeUpload(t.Context(), &graph.UploadSession{UploadURL: session.UploadURL}, strings.NewReader(content), size, nil)
		require.NoError(t, err, "%s: the part-uploaded session resumes once the storm passes", name)
		assert.Equal(t, content, remote.tree()[name], "%s: the resumed file is whole", name)

		var sent int64
		for _, fragment := range remote.fragmentsReceived(session.UploadURL) {
			assert.True(t, fragment.accepted, "%s: the fragment at %d resent accepted bytes", name, fragment.offset)
			sent += fragment.size
		}
		assert.Equal(t, size, sent, "%s: every byte reached the session once", name)
	}

	require.Positive(t, resumed, "no fragment fault left a part-uploaded session in %d attempts", attempts)
	c.mu.Lock()
	defer c.mu.Unlock()
	assert.Equal(t, resumed, c.faults[chaosOpSessionUpload+chaosPartSentSuffix], "every part-uploaded session is a counted fault")
}
