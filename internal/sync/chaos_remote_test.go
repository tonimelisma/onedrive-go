package sync

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	stdsync "sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
)

// Chaos Graph layer (D-092). chaosGraph wraps the scenario fake's client and,
// while a storm is on, makes the calls fail the ways Graph does, by a seed:
//
//   - before the call reaches OneDrive: 429 and 503 with Retry-After, 500,
//     502, 504, 408 and a connection reset;
//   - after it took effect ("succeeded but the response was lost"): the
//     mutation happened, the client sees a 504 or a reset;
//   - an upload session that times out after its empty item exists, some of
//     them after moving that item's eTag without content, as opening and
//     canceling a session does live (churnPlaceholder); and a download that
//     delivers part of the bytes and then drops;
//   - a create whose own answer is lost: the empty item exists, the client
//     never learned its ID, and the failure says only that the outcome is
//     unknown (graph.ErrCreateOutcomeUnknown), as does a create that failed
//     before it reached OneDrive without a definite refusal; and a create
//     that never left the client, which made nothing and carries no mark
//     (chaosAnswer holds which answers carry it);
//   - delta: a throttled or failing page (the first or a later one), 410 on a
//     saved token, a window split over several pages with items repeated on
//     an earlier page than their last occurrence, as Graph documents ("the
//     same item may appear more than once; use the last occurrence"), on top
//     of the fake's seeded order within a page;
//   - another client writing to OneDrive between two of the engine's calls.
//
// Faults stay inside Graph's documented guarantees: a page never loses an
// item, a repeated item's last occurrence is its current state, and every
// fault is transient, so once the storm passes sync must converge with every
// invariant holding (assertSyncInvariants).
//
// A stream is one operation on one key: get-item on an item ID, upload on a
// parent ID and a name, delta on a saved token, delta-next-page on a nextLink
// the layer made, and so on, as each caller of chaosGraph.call names it. A
// call's number is how many calls of its stream the layer saw earlier. A step
// is what startStep begins, one pass in the model: the storm is on or off for
// the whole step, and the first modelChaosPasses passes of every loop the
// model syncs to quiet in (the initial one, each batch's, and the one after a
// crash) are storm steps, the rest calm. Each call draws from a generator of
// its own, seeded from the seed, its stream and its call number, and draws
// only in a storm; a split window's nextLinks name the call that split it
// (chaosCall.pageTokenBase). The fault log (chaosDraw, drawLog) records each
// call's opening, every value it drew and every fault it got, under the
// call's stream, call number, step and storm flag.
//
// A seed fixes the values a call draws, in their order, for a given stream,
// call number and storm state, however the calls of different streams
// interleave (TestChaosGraph_StreamDrawsIgnoreHowStreamsInterleave). It fixes
// no more than that:
//   - how many values a call draws depends on what the call meets in the
//     fake, which the calls of other streams change: a split draws by the
//     size of its page, and the roll for a lost answer is drawn only when the
//     inner call succeeded;
//   - what a value picks can depend on the fake too: another client's write
//     draws from a fixed range, and the value picks among the files the tree
//     holds at that moment;
//   - which stream a delta request falls in depends on the order of the
//     fake's mutations: a delta token names a position in the drive's change
//     log, which is the order of every mutation. Item IDs and upload URLs are
//     named from what they name (nextID, openSessionLocked), not counted in
//     the order requests arrive;
//   - two calls of one stream that are in flight together take their numbers
//     in the order they reach the layer;
//   - which requests the client sends in which step is the client's own: it
//     releases retry work by elapsed time, which a one-shot run measures on
//     its bubble's virtual clock (engine_model_virtual_time_test.go), so the
//     host's speed no longer moves it.
//
// None of these is a random draw: every jitter of the run comes from a
// seeded source (the model clock, retry.JitterSource), and a one-shot run
// waits in virtual time, and the model's dispatch shuffle permutes a
// canonical order of each frontier. What stays outside the seed is the real
// filesystem: the kernel reuses a deleted file's inode for a new file, which
// the planner then reads as carrying the deleted file's content (an ordering
// edge that comes and goes), and the scan reads the kernel's timestamps. So
// a seed usually replays its run, which TestModel_ChaosSameSeedReplaysIdentically
// holds over twenty runs of each seed the chaos tests use; the first entry
// two runs differ in names what is left when it fails. Seed 50 does not
// replay. The `seed-replay:` line a failing chaos run logs
// (logSeedReplayOnFailure) reruns the seed.

const (
	chaosFaultOdds         = 4 // one call in chaosFaultOdds fails while a storm is on
	chaosConcurrentOdds    = 12
	chaosMaxRetryAfter     = 30 * time.Second
	chaosPageSplitMinItems = 3
	chaosPageTokenPrefix   = "chaos-page-"
	chaosOpDeltaNextPage   = "delta-next-page"
	chaosSeedEnv           = "ONEDRIVE_GO_CHAOS_SEEDS"
	chaosSeedStartEnv      = "ONEDRIVE_GO_CHAOS_SEED_START"
	chaosDefaultSeeds      = 2
	// chaosConcurrentWriteChoices is the fixed range another client's write
	// draws from, larger than any tree the model builds.
	chaosConcurrentWriteChoices = 64
)

type chaosGraph struct {
	seed uint64

	mu    stdsync.Mutex // guards step, storm, calls, pages, faults and draws
	storm bool
	// step counts the steps begun (startStep); the fault log names the step
	// each call opened in.
	step int
	// calls numbers the calls per operation and key, so each call's draws
	// come from its own seeded generator: the same seed, in the same storm
	// state, draws the same for the same call whatever order callers of
	// different streams reach the layer in.
	calls map[string]uint64
	// pages holds the rest of a split delta window by its synthetic nextLink
	// (chaosPendingWindow.token).
	pages map[string]chaosPendingWindow
	// concurrentWrite, when set, is another client's edit on OneDrive; the
	// layer runs it before a call now and then during a storm, with that
	// call's stream to choose what to write.
	concurrentWrite func(cc *chaosCall)
	// churnPlaceholder, when set, moves an item's eTag on OneDrive without
	// touching its content (moveScenarioRemote.churnETag). The layer runs it
	// for some of the creates whose session times out after the empty item
	// exists, so the eTag that moved without content (e2e
	// TestE2E_SyncWatch_FileModification), reachable before only through the
	// hand-set churnInterruptedCreates, is in the seeded storm.
	churnPlaceholder func(itemID string)
	// faults counts injected faults by kind, for the test to show the storm
	// really happened.
	faults map[string]int
	// draws is the fault log (chaosDraw): every call's opening, every value
	// it drew and every fault it got, in the order they happened.
	draws []chaosDraw
}

// chaosDraw is one entry of the fault log: the call it belongs to (its
// stream, an operation on a key, and its call number in that stream), the
// step it opened in and whether that step was a storm, and the outcome: the
// call opening, one value it drew, or one fault it got.
type chaosDraw struct {
	op      string
	key     string
	call    uint64
	step    int
	storm   bool
	outcome string
}

// stream names the stream the entry belongs to: its operation and key.
func (d chaosDraw) stream() string { return d.op + " " + strconv.Quote(d.key) }

// String is the entry within its stream: everything but the stream's name.
func (d chaosDraw) String() string {
	return fmt.Sprintf("call %d step %d storm=%t %s", d.call, d.step, d.storm, d.outcome)
}

// chaosPendingWindow is what remains of a split delta window: the pages not
// yet served, the first of them at index within the window, the real
// deltaLink, and splitBy, the start of the window's nextLinks, which names
// the call that split it (chaosCall.pageTokenBase).
type chaosPendingWindow struct {
	splitBy   string
	index     int
	pages     [][]graph.Item
	deltaLink string
}

func newChaosGraph(seed uint64) *chaosGraph {
	return &chaosGraph{
		seed:   seed,
		calls:  map[string]uint64{},
		pages:  map[string]chaosPendingWindow{},
		faults: map[string]int{},
	}
}

// startStep begins the next step, in a storm or calm: every call opened from
// now until the next startStep is drawn under this step's number and storm
// flag.
func (c *chaosGraph) startStep(storm bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.step++
	c.storm = storm
}

func (c *chaosGraph) faultCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, n := range c.faults {
		total += n
	}

	return total
}

func (c *chaosGraph) faultSummary() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	parts := make([]string, 0, len(c.faults))
	for kind, n := range c.faults {
		parts = append(parts, kind+"="+strconv.Itoa(n))
	}
	sort.Strings(parts)

	return strings.Join(parts, " ")
}

// chaosCall is one call's fault stream. Every draw and fault of the call goes
// into the layer's fault log. Outside a storm it never draws a fault.
type chaosCall struct {
	layer *chaosGraph
	op    string
	key   string
	n     uint64
	step  int
	rng   *rand.Rand
	storm bool
}

// call opens the fault stream of the next call of op on key.
func (c *chaosGraph) call(op, key string) *chaosCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := op + "\x00" + key
	n := c.calls[id]
	c.calls[id] = n + 1

	h := fnv.New64a()
	_, _ = h.Write([]byte(id))

	cc := &chaosCall{
		layer: c, op: op, key: key, n: n, step: c.step,
		rng:   rand.New(rand.NewPCG(c.seed^h.Sum64(), n)), //nolint:gosec // deterministic test RNG
		storm: c.storm,
	}
	c.draws = append(c.draws, cc.logEntry("open"))

	return cc
}

// logEntry is the fault log entry of the call with outcome.
func (cc *chaosCall) logEntry(outcome string) chaosDraw {
	return chaosDraw{op: cc.op, key: cc.key, call: cc.n, step: cc.step, storm: cc.storm, outcome: outcome}
}

// roll reports, during a storm, whether this call gets a fault.
func (cc *chaosCall) roll(odds int) bool {
	if !cc.storm {
		return false
	}
	drawn := cc.rng.IntN(odds)
	cc.record("roll/" + strconv.Itoa(odds) + "=" + strconv.Itoa(drawn))

	return drawn == 0
}

func (cc *chaosCall) intN(n int) int {
	drawn := cc.rng.IntN(n)
	cc.record("intN/" + strconv.Itoa(n) + "=" + strconv.Itoa(drawn))

	return drawn
}

// fault counts a fault of kind and logs it against the call.
func (cc *chaosCall) fault(kind string) {
	c := cc.layer
	c.mu.Lock()
	defer c.mu.Unlock()
	c.faults[kind]++
	c.draws = append(c.draws, cc.logEntry("fault "+kind))
}

// record appends outcome to the fault log under the call's identity.
func (cc *chaosCall) record(outcome string) {
	c := cc.layer
	c.mu.Lock()
	defer c.mu.Unlock()
	c.draws = append(c.draws, cc.logEntry(outcome))
}

// drawLog is a copy of the fault log.
func (c *chaosGraph) drawLog() []chaosDraw {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]chaosDraw(nil), c.draws...)
}

// maybeConcurrentWrite lets another client change OneDrive before a call.
func (c *chaosGraph) maybeConcurrentWrite(cc *chaosCall) {
	if c.concurrentWrite == nil || !cc.roll(chaosConcurrentOdds) {
		return
	}
	cc.fault("concurrent-write")
	c.concurrentWrite(cc)
}

// chaosAnswer is one way a call fails in the storm: an answer Graph gives, no
// answer at all (a dropped connection), or a request that never left the
// client. Which of them graph.CreateFile marks with
// graph.ErrCreateOutcomeUnknown, because they do not say whether the file was
// made, is written here once, in createMarked, and
// TestChaosCreateFault_MarksWhatTheGraphClientMarks feeds every answer through
// graph to hold the marks to its own classification.
type chaosAnswer struct {
	status     int    // the HTTP status; 0 for no answer
	code       string // Graph's error code, when it sends one
	sentinel   error  // the sentinel graph classifies the status as
	retryAfter bool   // the answer carries Retry-After
	// notSent is a request that never left the client: its token could not
	// be read, its context had already ended, or its retry after a refusal
	// was never sent.
	notSent      bool
	createMarked bool
}

// chaosTransientAnswers are the transient answers to a call that did not take
// effect, in the order the storm draws them.
func chaosTransientAnswers() []chaosAnswer {
	return []chaosAnswer{
		{status: http.StatusTooManyRequests, code: "activityLimitReached", sentinel: graph.ErrThrottled, retryAfter: true},
		{status: http.StatusServiceUnavailable, code: "serviceNotAvailable", sentinel: graph.ErrServerError, retryAfter: true, createMarked: true},
		{status: http.StatusInternalServerError, code: "generalException", sentinel: graph.ErrServerError, createMarked: true},
		{status: http.StatusBadGateway, sentinel: graph.ErrServerError, createMarked: true},
		{status: http.StatusGatewayTimeout, sentinel: graph.ErrServerError, createMarked: true},
		{status: http.StatusRequestTimeout, code: "timeout", createMarked: true},
		{createMarked: true}, // the connection reset before any answer
	}
}

// chaosCreateAnswers are a create's faults before it takes effect: the
// transient answers and a create that never left the client, which made
// nothing and says so.
func chaosCreateAnswers() []chaosAnswer {
	return append(chaosTransientAnswers(), chaosAnswer{notSent: true})
}

// chaosLostAnswers are what the client sees of a call that took effect and
// lost its answer: a gateway timeout or a reset.
func chaosLostAnswers() []chaosAnswer {
	var lost []chaosAnswer
	for _, answer := range chaosTransientAnswers() {
		if answer.status == http.StatusGatewayTimeout || answer.status == 0 {
			lost = append(lost, answer)
		}
	}

	return lost
}

// kind names the answer in a fault kind: its status, "reset" or "not-sent".
func (a chaosAnswer) kind() string {
	switch {
	case a.notSent:
		return "not-sent"
	case a.status == 0:
		return "reset"
	default:
		return strconv.Itoa(a.status)
	}
}

// failure is the error the client sees for the answer; how is "" for a
// call that never reached OneDrive, or what happened before its answer was
// lost.
func (a chaosAnswer) failure(op, how string, retryAfter time.Duration) error {
	switch {
	case a.notSent:
		return fmt.Errorf("chaos %s: the request never left the client: %w", op, errChaosNotSent)
	case a.status == 0:
		return fmt.Errorf("chaos %s%s: read tcp: %w", op, how, syscall.ECONNRESET)
	}
	graphErr := &graph.GraphError{StatusCode: a.status, Code: a.code, Err: a.sentinel}
	if a.retryAfter {
		graphErr.RetryAfter = retryAfter
	}

	return graphErr
}

// errChaosNotSent is the cause of a request the chaos layer never sent.
var errChaosNotSent = errors.New("chaos: the token source failed")

// transientError draws one of answers, seeded, as the failure of a call that
// did not take effect.
func (c *chaosGraph) transientError(cc *chaosCall, op string, answers []chaosAnswer) (chaosAnswer, error) {
	retryAfter := time.Duration(1+cc.intN(int(chaosMaxRetryAfter/time.Second))) * time.Second
	answer := answers[cc.intN(len(answers))]
	cc.fault(op + ":" + answer.kind())

	return answer, answer.failure(op, "", retryAfter)
}

// chaosResponseLostSuffix ends the fault kind of a request that took effect
// and lost its answer (lostResponseError). chaosOpCreate counts the faults of
// a small file's create request (uploadCreating): before it reached OneDrive,
// or with its answer lost (chaosCreateResponseLost).
const (
	chaosResponseLostSuffix = ":response-lost"
	chaosOpCreate           = "create"
	chaosCreateResponseLost = chaosOpCreate + chaosResponseLostSuffix
)

// lostResponseError is what the client sees when the request took effect but
// the answer never arrived.
func (c *chaosGraph) lostResponseError(cc *chaosCall, op string) (chaosAnswer, error) {
	cc.fault(op + chaosResponseLostSuffix)
	lost := chaosLostAnswers()
	answer := lost[cc.intN(len(lost))]

	return answer, answer.failure(op, " after it took effect", 0)
}

// wrap returns a client whose every call passes through the chaos layer.
func (c *chaosGraph) wrap(inner *syncfixture.EngineMockClient) *syncfixture.EngineMockClient {
	out := *inner
	out.DeltaFn = func(ctx context.Context, driveID driveid.ID, token string) (*graph.DeltaPage, error) {
		return c.delta(ctx, driveID, token, inner.DeltaFn)
	}
	out.GetItemFn = func(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
		if err := c.before(c.call("get-item", itemID), "get-item"); err != nil {
			return nil, err
		}
		return inner.GetItemFn(ctx, driveID, itemID)
	}
	out.ListChildrenFn = func(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error) {
		if err := c.before(c.call("list-children", parentID), "list-children"); err != nil {
			return nil, err
		}
		return inner.ListChildrenFn(ctx, driveID, parentID)
	}
	out.DownloadFn = func(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error) {
		return c.download(ctx, driveID, itemID, w, inner.DownloadFn)
	}
	out.MoveItemOptsFn = func(
		ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string, opts graph.MoveOptions,
	) (*graph.Item, error) {
		return chaosMutation(c, "move", itemID, func() (*graph.Item, error) {
			return inner.MoveItemOptsFn(ctx, driveID, itemID, newParentID, newName, opts)
		})
	}
	out.CreateFolderFn = func(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error) {
		return chaosMutation(c, "create-folder", parentID+"/"+name, func() (*graph.Item, error) {
			return inner.CreateFolderFn(ctx, driveID, parentID, name)
		})
	}
	out.DeleteItemIfMatchFn = func(ctx context.Context, driveID driveid.ID, itemID, ifMatch string) error {
		_, err := chaosMutation(c, "delete", itemID, func() (*graph.Item, error) {
			return nil, inner.DeleteItemIfMatchFn(ctx, driveID, itemID, ifMatch)
		})
		return err
	}
	out.UploadFn = func(
		ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc,
	) (*graph.Item, error) {
		return chaosMutation(c, "upload", parentID+"/"+name, func() (*graph.Item, error) {
			return inner.UploadFn(ctx, driveID, parentID, name, content, size, mtime, progress)
		})
	}
	out.UploadToItemFn = func(
		ctx context.Context, driveID driveid.ID, itemID, ifMatch string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc,
	) (*graph.Item, error) {
		return chaosMutation(c, "upload-to-item", itemID, func() (*graph.Item, error) {
			return inner.UploadToItemFn(ctx, driveID, itemID, ifMatch, content, size, mtime, progress)
		})
	}
	out.UploadCreatingFn = func(
		ctx context.Context, parentID, name string, content io.ReaderAt, size int64, onCreated graph.CreatedFunc,
	) (*graph.Item, error) {
		return c.uploadCreating(ctx, parentID, name, content, size, onCreated, inner.UploadCreatingFn)
	}

	return &out
}

// before is the fault a call gets before it reaches OneDrive, if any.
func (c *chaosGraph) before(cc *chaosCall, op string) error {
	_, err := c.beforeFrom(cc, op, chaosTransientAnswers())

	return err
}

// beforeFrom is before, drawing the fault from answers.
func (c *chaosGraph) beforeFrom(cc *chaosCall, op string, answers []chaosAnswer) (chaosAnswer, error) {
	c.maybeConcurrentWrite(cc)
	if !cc.roll(chaosFaultOdds) {
		return chaosAnswer{}, nil
	}

	return c.transientError(cc, op, answers)
}

// beforeCreate is before for a create, whose faults include one that never
// left the client; the failure reads as graph.CreateFile reports it.
func (c *chaosGraph) beforeCreate(cc *chaosCall, op, name string) error {
	answer, err := c.beforeFrom(cc, op, chaosCreateAnswers())
	if err != nil {
		return chaosCreateFailure(name, answer, err)
	}

	return nil
}

// chaosMutation runs a mutating call through the layer: it may fail before
// reaching OneDrive, or take effect and lose its response.
func chaosMutation[T any](c *chaosGraph, op, key string, call func() (T, error)) (T, error) {
	var none T
	cc := c.call(op, key)
	if err := c.before(cc, op); err != nil {
		return none, err
	}
	result, err := call()
	if err == nil && cc.roll(chaosFaultOdds) {
		_, lostErr := c.lostResponseError(cc, op)
		return none, lostErr
	}

	return result, err
}

// chaosCreateFunc is the scenario fake's two-step create.
type chaosCreateFunc func(context.Context, string, string, io.ReaderAt, int64, graph.CreatedFunc) (*graph.Item, error)

// uploadCreating may fail after the create took effect, in the two ways a
// create does. The session may time out after the client learned of the empty
// item, as a connection lost mid-session does: the item exists, the content
// does not, and for some of them its eTag moved meanwhile
// (chaosPlaceholderChurned). Or the creating request's own answer is lost
// (createResponseLost). A create that fails before it reaches OneDrive, or
// never leaves the client, reads as graph.CreateFile reports it
// (chaosCreateFailure). A fault of the create itself counts under
// chaosOpCreate, the session's under "upload-session"; the call's fault
// stream keeps its "upload-session" name.
func (c *chaosGraph) uploadCreating(
	ctx context.Context, parentID, name string, content io.ReaderAt, size int64, onCreated graph.CreatedFunc,
	inner chaosCreateFunc,
) (*graph.Item, error) {
	cc := c.call("upload-session", parentID+"/"+name)
	if err := c.beforeCreate(cc, chaosOpCreate, name); err != nil {
		return nil, err
	}
	if !cc.roll(chaosFaultOdds) {
		return inner(ctx, parentID, name, content, size, onCreated)
	}
	if cc.intN(2) == 0 {
		return c.createResponseLost(ctx, cc, parentID, name, content, size, inner)
	}

	cc.fault(chaosSessionTimedOut)
	timeout := &graph.GraphError{StatusCode: http.StatusGatewayTimeout, Err: graph.ErrServerError}
	churn := c.churnPlaceholder != nil && cc.intN(2) == 0

	return inner(ctx, parentID, name, content, size, func(created *graph.Item) error {
		if onCreated != nil {
			if err := onCreated(created); err != nil {
				return err
			}
		}
		if churn {
			cc.fault(chaosPlaceholderChurned)
			c.churnPlaceholder(created.ID)
		}
		return timeout
	})
}

// The fault kinds of a session that timed out after its empty item existed:
// every one of them, and those whose item's eTag moved without content before
// the client gave up.
const (
	chaosSessionTimedOut    = "upload-session:timeout-after-create"
	chaosPlaceholderChurned = chaosSessionTimedOut + ":etag-moved"
)

// createResponseLost runs the create and loses its answer: the empty item
// exists on OneDrive, the client's create hook never runs (the client never
// learned the item's ID), and what comes back is the failure of a request
// whose outcome is unknown. A create that made nothing (the name was taken)
// keeps its own answer.
func (c *chaosGraph) createResponseLost(
	ctx context.Context, cc *chaosCall, parentID, name string, content io.ReaderAt, size int64, inner chaosCreateFunc,
) (*graph.Item, error) {
	created := false
	item, err := inner(ctx, parentID, name, content, size, func(*graph.Item) error {
		created = true
		return errors.New("chaos: stopped after the create, before the client learned of it")
	})
	if !created {
		return item, err
	}

	answer, lostErr := c.lostResponseError(cc, chaosOpCreate)

	return nil, chaosCreateFailure(name, answer, lostErr)
}

// chaosCreateFailure is a failed create as graph.CreateFile reports it: the
// answer's createMarked says whether it carries graph.ErrCreateOutcomeUnknown.
// An answer that does not say whether the file was made is marked whether or
// not the request reached OneDrive, because the client cannot tell those
// apart; a refusal and a create that never left are not.
func chaosCreateFailure(name string, answer chaosAnswer, err error) error {
	if answer.createMarked {
		return fmt.Errorf("graph: creating file %q: %w: %w", name, graph.ErrCreateOutcomeUnknown, err)
	}

	return fmt.Errorf("graph: creating file %q: %w", name, err)
}

// download may deliver part of the bytes before the connection drops.
func (c *chaosGraph) download(
	ctx context.Context, driveID driveid.ID, itemID string, w io.Writer,
	inner func(context.Context, driveid.ID, string, io.Writer) (int64, error),
) (int64, error) {
	cc := c.call("download", itemID)
	if err := c.before(cc, "download"); err != nil {
		return 0, err
	}
	if !cc.roll(chaosFaultOdds) {
		return inner(ctx, driveID, itemID, w)
	}

	cc.fault("download:truncated")
	var full strings.Builder
	if _, err := inner(ctx, driveID, itemID, &full); err != nil {
		return 0, err
	}
	half := full.String()[:full.Len()/2]
	n, err := io.WriteString(w, half)
	if err != nil {
		return int64(n), fmt.Errorf("write chaos download: %w", err)
	}

	return int64(n), fmt.Errorf("chaos download: read tcp: %w", syscall.ECONNRESET)
}

// delta serves one page. A storm may fail the page, expire a saved token, or
// split the window into several pages with repeated items.
func (c *chaosGraph) delta(
	ctx context.Context, driveID driveid.ID, token string,
	inner func(context.Context, driveid.ID, string) (*graph.DeltaPage, error),
) (*graph.DeltaPage, error) {
	if strings.HasPrefix(token, chaosPageTokenPrefix) {
		if err := c.before(c.call(chaosOpDeltaNextPage, token), chaosOpDeltaNextPage); err != nil {
			return nil, err
		}
		return c.nextPage(token)
	}
	cc := c.call("delta", token)
	if err := c.before(cc, "delta"); err != nil {
		return nil, err
	}
	if token != "" && cc.roll(chaosFaultOdds) {
		cc.fault("delta:410")
		return nil, &graph.GraphError{StatusCode: http.StatusGone, Code: "resyncRequired", Err: graph.ErrGone}
	}

	page, err := inner(ctx, driveID, token)
	if err != nil || len(page.Items) < chaosPageSplitMinItems || !cc.roll(2) {
		return page, err
	}

	return c.split(cc, page), nil
}

// split spreads a window over two to four pages and repeats some items on a
// page before the one holding their last occurrence.
func (c *chaosGraph) split(cc *chaosCall, page *graph.DeltaPage) *graph.DeltaPage {
	items := page.Items
	pageCount := 2 + cc.intN(3)
	pages := make([][]graph.Item, pageCount)
	for i := range items {
		pages[i*pageCount/len(items)] = append(pages[i*pageCount/len(items)], items[i])
	}
	for p := 1; p < pageCount; p++ {
		for i := range pages[p] {
			if pages[p][i].IsRoot || cc.intN(3) != 0 {
				continue
			}
			earlier := cc.intN(p)
			pages[earlier] = append(pages[earlier], pages[p][i])
		}
	}
	cc.fault(fmt.Sprintf("delta:split-%d", pageCount))

	return c.servePages(chaosPendingWindow{splitBy: cc.pageTokenBase(), pages: pages, deltaLink: page.DeltaLink})
}

// pageTokenBase starts the nextLinks of a window this call split. It names the
// call, by its stream and call number, so a follow-up page's token, and the
// fault stream that token keys, depend on this call alone and on no other
// stream's traffic.
func (cc *chaosCall) pageTokenBase() string {
	return chaosPageTokenPrefix + cc.op + "/" + cc.key + "/call-" + strconv.FormatUint(cc.n, 10)
}

// token is the nextLink that serves the window's first remaining page.
func (w chaosPendingWindow) token() string {
	return w.splitBy + "/page-" + strconv.Itoa(w.index)
}

// servePages serves the window's first remaining page and keeps the rest under
// the next page's token.
func (c *chaosGraph) servePages(window chaosPendingWindow) *graph.DeltaPage {
	if len(window.pages) == 1 {
		return &graph.DeltaPage{Items: window.pages[0], DeltaLink: window.deltaLink}
	}
	rest := chaosPendingWindow{splitBy: window.splitBy, index: window.index + 1, pages: window.pages[1:], deltaLink: window.deltaLink}
	next := rest.token()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pages[next] = rest

	return &graph.DeltaPage{Items: window.pages[0], NextLink: next}
}

// nextPage serves the page a nextLink of servePages names. Serving one token
// again serves the same page and the same next token.
func (c *chaosGraph) nextPage(token string) (*graph.DeltaPage, error) {
	c.mu.Lock()
	window, ok := c.pages[token]
	c.mu.Unlock()
	if !ok {
		return nil, &graph.GraphError{StatusCode: http.StatusBadRequest, Code: "invalidRequest", Err: graph.ErrBadRequest}
	}

	return c.servePages(window), nil
}

// concurrentRemoteWrite is another client overwriting a file on OneDrive
// while the engine is mid-pass: between the observation that planned an
// action and the call that carries it out. The call draws one value from a
// fixed range (chaosConcurrentWriteChoices) before it reads the tree, so
// whether it draws and what it draws do not follow the fake; only the file
// the value picks among the files the tree holds at that moment does, and on
// an empty tree it writes nothing. The fault log names the file.
func (r *modelRun) concurrentRemoteWrite(cc *chaosCall) {
	choice := cc.intN(chaosConcurrentWriteChoices)
	files := modelEntriesOf(r.remote.tree(), false)
	if len(files) == 0 {
		return
	}
	p := files[choice%len(files)]
	cc.record("concurrent-write " + p)
	content := r.nextContent("concurrent")
	r.remote.putFileAt(r.t, p, content)

	r.concurrentMu.Lock()
	defer r.concurrentMu.Unlock()
	r.logf("concurrent remote write %s", p)
	r.concurrentEdits = append(r.concurrentEdits, syncfixture.ModelEdit{Side: syncfixture.ModelRemoteSide, Kind: modelOpConcurrentWrite, Path: p, Content: content})
}

// takeConcurrentEdits returns and clears the edits the chaos layer's other
// client made, so the batch's no-loss check covers them.
func (r *modelRun) takeConcurrentEdits() []syncfixture.ModelEdit {
	r.concurrentMu.Lock()
	defer r.concurrentMu.Unlock()
	edits := r.concurrentEdits
	r.concurrentEdits = nil

	return edits
}

// chaosRaceSeed is the one seed of the chaos run under -race. Its
// modelRaceBatches batches draw both faults of a create that took effect: a
// create whose own answer was lost and a session that timed out once the
// empty item existed. The run asserts that the seed still draws both, so a
// change that renumbers what a seed does cannot quietly drop them. It is the
// first seed that draws both since item IDs and upload URLs are named from
// what they name (the seeds below it draw one or neither).
const chaosRaceSeed = 79

// The draws are those of a byte-exact volume, as Linux's are: the model's
// case-variant creates are one entry on a case-insensitive volume (macOS by
// default), the run takes another course there, and seed 79 drew no
// timed-out session on macOS while it converged as it must. The run checks
// the draws only where the seed was chosen, and every model check
// everywhere.
//
// chaosRaceSeedFaults are the fault kinds chaosRaceSeed must draw.
func chaosRaceSeedFaults() []string {
	return []string{chaosCreateResponseLost, chaosSessionTimedOut}
}

// Validates: R-2.3.1, R-6.8.15
//
// The model test through the chaos Graph layer: every batch's first passes
// run in a storm of seeded transient faults, lost responses, interrupted
// sessions, truncated downloads, odd delta windows and concurrent writers;
// once it passes, sync converges with every model check and invariant
// holding. Each seed logs its fault summary when it ends, failing or not, and
// a failing one its `seed-replay:` line. ONEDRIVE_GO_CHAOS_SEEDS widens the
// search, from ONEDRIVE_GO_CHAOS_SEED_START (or name one seed with
// ONEDRIVE_GO_MODEL_SEED).
func TestModel_ChaosGraphConverges(t *testing.T) {
	syncfixture.SkipInShort(t)
	t.Parallel()

	for _, seed := range chaosSeeds(t) {
		t.Run("seed="+strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()
			syncfixture.LogSeedReplayOnFailure(t, modelSeedEnv, seed)
			run := runModelScenarioWith(t, seed, chaosRunOptions())
			require.Positive(t, run.chaos.faultCount(), "the storm injected no fault")
			if seed != chaosRaceSeed || volumeFoldsCase(t) {
				return
			}
			run.chaos.mu.Lock()
			defer run.chaos.mu.Unlock()
			for _, kind := range chaosRaceSeedFaults() {
				assert.Positive(t, run.chaos.faults[kind], "seed %d drew no %s fault", seed, kind)
			}
		})
	}
}

// chaosRunOptions are the options of every chaos model run: the chaos layer,
// over chaosBatches batches.
func chaosRunOptions() modelOptions {
	return modelOptions{chaos: true, batches: chaosBatches(syncfixture.RaceDetectorEnabled)}
}

// chaosBatches is the batch count of a chaos run: modelRaceBatches under the
// race detector, modelBatches without it.
func chaosBatches(race bool) int {
	if race {
		return modelRaceBatches
	}

	return modelBatches
}

// chaosSeedVars are the chaos run's variables; its one named seed is the
// engine run's variable.
func chaosSeedVars() syncfixture.SeedVars {
	return syncfixture.SeedVars{Single: modelSeedEnv, Count: chaosSeedEnv, Start: chaosSeedStartEnv}
}

// chaosSeeds is the chaos run's seeds: the ones the environment names, else
// the first chaosDefaultSeeds and chaosRaceSeed (chaosRaceSeed alone under
// -race).
func chaosSeeds(t *testing.T) []uint64 {
	t.Helper()

	if chaosSeedVars().SetIn(os.Getenv) {
		count := chaosDefaultSeeds
		if syncfixture.RaceDetectorEnabled {
			count = 1
		}
		return syncfixture.SeedsFromEnv(t, chaosSeedVars(), count)
	}
	if syncfixture.RaceDetectorEnabled {
		return []uint64{chaosRaceSeed}
	}

	return append(syncfixture.SeedsFromEnv(t, chaosSeedVars(), chaosDefaultSeeds), chaosRaceSeed)
}

// Validates: R-5.6.15
//
// The chaos layer's lost create answer is the loss it models: the file exists
// on OneDrive, empty; the client's create hook never ran; and the failure
// carries graph.ErrCreateOutcomeUnknown. A create that failed before it
// reached OneDrive carries the same mark and leaves nothing behind.
func TestChaosGraph_LostCreateAnswerLeavesTheFileAndNeverRunsTheHook(t *testing.T) {
	t.Parallel()

	const (
		attempts = 200
		content  = "my words\n"
	)
	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	c := newChaosGraph(1)
	c.startStep(true)
	client := c.wrap(remote.client(t))

	lost := 0
	for i := range attempts {
		name := fmt.Sprintf("doc-%d.txt", i)
		hookRan := false
		_, err := client.UploadCreatingFn(t.Context(), syncexec.GraphRootID, name, strings.NewReader(content), int64(len(content)),
			func(*graph.Item) error {
				hookRan = true
				return nil
			})
		if !errors.Is(err, graph.ErrCreateOutcomeUnknown) {
			continue
		}
		assert.False(t, hookRan, "%s: the client learned of a create whose outcome is unknown", name)
		if _, landed := remote.itemAt(name); !landed {
			continue // failed before it reached OneDrive
		}
		lost++
		assert.Empty(t, remote.tree()[name], "%s: a create alone carries no content", name)
	}

	require.Positive(t, lost, "no create lost its answer in %d attempts", attempts)
	c.mu.Lock()
	defer c.mu.Unlock()
	assert.Equal(t, lost, c.faults[chaosCreateResponseLost])
}

// Validates: R-5.6.15
//
// The churn variant of the timed-out session is the fault it models: the
// empty item exists, the client's hook ran with its creation eTag, and
// OneDrive now holds it empty under another eTag. The other timed-out
// sessions leave the creation eTag, so the storm has both.
func TestChaosGraph_TimeoutAfterCreateCanMoveThePlaceholderETag(t *testing.T) {
	t.Parallel()

	const (
		attempts = 200
		content  = "my words\n"
	)
	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	c := newChaosGraph(1)
	c.churnPlaceholder = remote.churnETag
	c.startStep(true)
	client := c.wrap(remote.client(t))

	moved, kept := 0, 0
	for i := range attempts {
		name := fmt.Sprintf("doc-%d.txt", i)
		var recorded *graph.Item
		_, err := client.UploadCreatingFn(t.Context(), syncexec.GraphRootID, name, strings.NewReader(content), int64(len(content)),
			func(created *graph.Item) error {
				recorded = created
				return nil
			})
		if err == nil || recorded == nil {
			continue // succeeded, or failed before the client learned of an item
		}
		require.ErrorIs(t, err, graph.ErrServerError, "%s: the session timed out", name)
		live, ok := remote.itemAt(name)
		require.True(t, ok, "%s: the empty item exists", name)
		assert.Zero(t, live.Size, "%s: no content landed", name)
		if live.ETag == recorded.ETag {
			kept++
		} else {
			moved++
		}
	}

	require.Positive(t, moved, "no placeholder's eTag moved in %d attempts", attempts)
	require.Positive(t, kept, "every timed-out placeholder's eTag moved in %d attempts", attempts)
	c.mu.Lock()
	defer c.mu.Unlock()
	assert.Equal(t, moved, c.faults[chaosPlaceholderChurned])
	assert.Equal(t, moved+kept, c.faults[chaosSessionTimedOut])
}

// Validates: R-2.15.2
//
// A split window keeps every item and puts each repeated item's last
// occurrence where the unsplit page had it.
func TestChaosGraph_SplitKeepsLastOccurrence(t *testing.T) {
	t.Parallel()

	for seed := uint64(1); seed <= 20; seed++ {
		c := newChaosGraph(seed)
		c.startStep(true)
		items := []graph.Item{{ID: "root", IsRoot: true}}
		for i := range 9 {
			items = append(items, graph.Item{ID: fmt.Sprintf("item-%d", i), Name: fmt.Sprintf("n%d", i)})
		}
		first := c.split(c.call("delta", "seq-0"), &graph.DeltaPage{Items: items, DeltaLink: "seq-9"})

		var served []graph.Item
		page := first
		for pages := 0; ; pages++ {
			require.Less(t, pages, 5, "seed %d: too many pages", seed)
			served = append(served, page.Items...)
			if page.DeltaLink != "" {
				assert.Equal(t, "seq-9", page.DeltaLink)
				break
			}
			next, err := c.nextPage(page.NextLink)
			require.NoError(t, err)
			page = next
		}

		lastAt := map[string]int{}
		for i := range served {
			lastAt[served[i].ID] = i
		}
		require.Len(t, lastAt, len(items), "seed %d: an item was lost", seed)
		order := make([]string, 0, len(items))
		for i := range served {
			if lastAt[served[i].ID] == i {
				order = append(order, served[i].ID)
			}
		}
		want := make([]string, 0, len(items))
		for i := range items {
			want = append(want, items[i].ID)
		}
		assert.Equal(t, want, order, "seed %d: last occurrences out of order", seed)
	}
}

// volumeFoldsCase reports whether the volume t's temporary folders are on
// finds a name in another case: a case-insensitive volume, as macOS's is by
// default.
func volumeFoldsCase(t *testing.T) bool {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "probe"), nil, 0o600))
	_, err := os.Lstat(filepath.Join(dir, "PROBE"))
	if err == nil {
		return true
	}
	require.ErrorIs(t, err, os.ErrNotExist)

	return false
}
