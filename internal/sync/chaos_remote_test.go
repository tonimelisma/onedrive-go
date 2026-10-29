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
)

// Chaos Graph layer (D-092). chaosGraph wraps the scenario fake's client and,
// while a storm is on, makes the calls fail the ways Graph does, by a seed:
//
//   - before the call reaches OneDrive: 429 and 503 with Retry-After, 500,
//     502, 504, 408 and a connection reset;
//   - after it took effect ("succeeded but the response was lost"): the
//     mutation happened, the client sees a 504 or a reset;
//   - an upload session that times out after its empty item exists, and a
//     download that delivers part of the bytes and then drops;
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

const (
	chaosFaultOdds         = 4 // one call in chaosFaultOdds fails while a storm is on
	chaosConcurrentOdds    = 12
	chaosMaxRetryAfter     = 30 * time.Second
	chaosPageSplitMinItems = 3
	chaosPageTokenPrefix   = "chaos-page-"
	chaosSeedEnv           = "ONEDRIVE_GO_CHAOS_SEEDS"
	chaosDefaultSeeds      = 2
)

type chaosGraph struct {
	seed uint64

	mu    stdsync.Mutex // guards storm, calls, pages, pageSeq and faults
	storm bool
	// calls numbers the calls per operation and key, so each call's faults
	// come from its own seeded stream: the same seed injects the same faults
	// into the same calls whatever order concurrent callers reach the layer.
	calls map[string]uint64
	// pages holds the rest of a split delta window by its synthetic
	// nextLink: each entry is the remaining pages and the real deltaLink.
	pages   map[string]chaosPendingWindow
	pageSeq int
	// concurrentWrite, when set, is another client's edit on OneDrive; the
	// layer runs it before a call now and then during a storm, with that
	// call's stream to choose what to write.
	concurrentWrite func(rng *rand.Rand)
	// faults counts injected faults by kind, for the test to show the storm
	// really happened.
	faults map[string]int
}

type chaosPendingWindow struct {
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

func (c *chaosGraph) setStorm(on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storm = on
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

// chaosCall is one call's fault stream. Outside a storm it never draws a
// fault.
type chaosCall struct {
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

	return &chaosCall{
		rng:   rand.New(rand.NewPCG(c.seed^h.Sum64(), n)), //nolint:gosec // deterministic test RNG
		storm: c.storm,
	}
}

// roll reports, during a storm, whether this call gets a fault.
func (cc *chaosCall) roll(odds int) bool {
	return cc.storm && cc.rng.IntN(odds) == 0
}

func (cc *chaosCall) intN(n int) int {
	return cc.rng.IntN(n)
}

func (c *chaosGraph) count(kind string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.faults[kind]++
}

// maybeConcurrentWrite lets another client change OneDrive before a call.
func (c *chaosGraph) maybeConcurrentWrite(cc *chaosCall) {
	if c.concurrentWrite == nil || !cc.roll(chaosConcurrentOdds) {
		return
	}
	c.count("concurrent-write")
	c.concurrentWrite(cc.rng)
}

// transientError is one of the transient answers Graph gives, seeded.
func (c *chaosGraph) transientError(cc *chaosCall, op string) error {
	retryAfter := time.Duration(1+cc.intN(int(chaosMaxRetryAfter/time.Second))) * time.Second
	var err error
	switch cc.intN(7) {
	case 0:
		err = &graph.GraphError{StatusCode: http.StatusTooManyRequests, Code: "activityLimitReached", Err: graph.ErrThrottled, RetryAfter: retryAfter}
	case 1:
		err = &graph.GraphError{StatusCode: http.StatusServiceUnavailable, Code: "serviceNotAvailable", Err: graph.ErrServerError, RetryAfter: retryAfter}
	case 2:
		err = &graph.GraphError{StatusCode: http.StatusInternalServerError, Code: "generalException", Err: graph.ErrServerError}
	case 3:
		err = &graph.GraphError{StatusCode: http.StatusBadGateway, Err: graph.ErrServerError}
	case 4:
		err = &graph.GraphError{StatusCode: http.StatusGatewayTimeout, Err: graph.ErrServerError}
	case 5:
		err = &graph.GraphError{StatusCode: http.StatusRequestTimeout, Code: "timeout"}
	default:
		err = fmt.Errorf("chaos %s: read tcp: %w", op, syscall.ECONNRESET)
	}
	c.count(op + ":" + chaosErrorKind(err))

	return err
}

// lostResponseError is what the client sees when the request took effect but
// the answer never arrived.
func (c *chaosGraph) lostResponseError(cc *chaosCall, op string) error {
	c.count(op + ":response-lost")
	if cc.intN(2) == 0 {
		return &graph.GraphError{StatusCode: http.StatusGatewayTimeout, Err: graph.ErrServerError}
	}

	return fmt.Errorf("chaos %s after it took effect: read tcp: %w", op, syscall.ECONNRESET)
}

func chaosErrorKind(err error) string {
	var graphErr *graph.GraphError
	if errors.As(err, &graphErr) {
		return strconv.Itoa(graphErr.StatusCode)
	}

	return "reset"
}

// wrap returns a client whose every call passes through the chaos layer.
func (c *chaosGraph) wrap(inner *engineMockClient) *engineMockClient {
	out := *inner
	out.deltaFn = func(ctx context.Context, driveID driveid.ID, token string) (*graph.DeltaPage, error) {
		return c.delta(ctx, driveID, token, inner.deltaFn)
	}
	out.getItemFn = func(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
		if err := c.before(c.call("get-item", itemID), "get-item"); err != nil {
			return nil, err
		}
		return inner.getItemFn(ctx, driveID, itemID)
	}
	out.getItemByPathFn = func(ctx context.Context, driveID driveid.ID, remotePath string) (*graph.Item, error) {
		if err := c.before(c.call("get-by-path", remotePath), "get-by-path"); err != nil {
			return nil, err
		}
		return inner.getItemByPathFn(ctx, driveID, remotePath)
	}
	out.listChildrenFn = func(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error) {
		if err := c.before(c.call("list-children", parentID), "list-children"); err != nil {
			return nil, err
		}
		return inner.listChildrenFn(ctx, driveID, parentID)
	}
	out.downloadFn = func(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error) {
		return c.download(ctx, driveID, itemID, w, inner.downloadFn)
	}
	out.moveItemOptsFn = func(
		ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string, opts graph.MoveOptions,
	) (*graph.Item, error) {
		return chaosMutation(c, "move", itemID, func() (*graph.Item, error) {
			return inner.moveItemOptsFn(ctx, driveID, itemID, newParentID, newName, opts)
		})
	}
	out.createFolderFn = func(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error) {
		return chaosMutation(c, "create-folder", parentID+"/"+name, func() (*graph.Item, error) {
			return inner.createFolderFn(ctx, driveID, parentID, name)
		})
	}
	out.deleteItemIfMatchFn = func(ctx context.Context, driveID driveid.ID, itemID, ifMatch string) error {
		_, err := chaosMutation(c, "delete", itemID, func() (*graph.Item, error) {
			return nil, inner.deleteItemIfMatchFn(ctx, driveID, itemID, ifMatch)
		})
		return err
	}
	out.uploadFn = func(
		ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc,
	) (*graph.Item, error) {
		return chaosMutation(c, "upload", parentID+"/"+name, func() (*graph.Item, error) {
			return inner.uploadFn(ctx, driveID, parentID, name, content, size, mtime, progress)
		})
	}
	out.uploadToItemFn = func(
		ctx context.Context, driveID driveid.ID, itemID, ifMatch string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc,
	) (*graph.Item, error) {
		return chaosMutation(c, "upload-to-item", itemID, func() (*graph.Item, error) {
			return inner.uploadToItemFn(ctx, driveID, itemID, ifMatch, content, size, mtime, progress)
		})
	}
	out.uploadCreatingFn = func(
		ctx context.Context, parentID, name string, content io.ReaderAt, size int64, onCreated graph.CreatedFunc,
	) (*graph.Item, error) {
		return c.uploadCreating(ctx, parentID, name, content, size, onCreated, inner.uploadCreatingFn)
	}

	return &out
}

// before is the fault a call gets before it reaches OneDrive, if any.
func (c *chaosGraph) before(cc *chaosCall, op string) error {
	c.maybeConcurrentWrite(cc)
	if !cc.roll(chaosFaultOdds) {
		return nil
	}

	return c.transientError(cc, op)
}

// chaosMutation runs a mutating call through the layer: it may fail before
// reaching OneDrive, or take effect and lose its response.
func chaosMutation(c *chaosGraph, op, key string, call func() (*graph.Item, error)) (*graph.Item, error) {
	cc := c.call(op, key)
	if err := c.before(cc, op); err != nil {
		return nil, err
	}
	item, err := call()
	if err == nil && cc.roll(chaosFaultOdds) {
		return nil, c.lostResponseError(cc, op)
	}

	return item, err
}

// uploadCreating may time out after the session created the empty item, as
// a connection lost mid-session does: the item exists, the content does not.
func (c *chaosGraph) uploadCreating(
	ctx context.Context, parentID, name string, content io.ReaderAt, size int64, onCreated graph.CreatedFunc,
	inner func(context.Context, string, string, io.ReaderAt, int64, graph.CreatedFunc) (*graph.Item, error),
) (*graph.Item, error) {
	cc := c.call("upload-session", parentID+"/"+name)
	if err := c.before(cc, "upload-session"); err != nil {
		return nil, err
	}
	if !cc.roll(chaosFaultOdds) {
		return inner(ctx, parentID, name, content, size, onCreated)
	}

	c.count("upload-session:timeout-after-create")
	timeout := &graph.GraphError{StatusCode: http.StatusGatewayTimeout, Err: graph.ErrServerError}

	return inner(ctx, parentID, name, content, size, func(created *graph.Item) error {
		if onCreated != nil {
			if err := onCreated(created); err != nil {
				return err
			}
		}
		return timeout
	})
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

	c.count("download:truncated")
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
		if err := c.before(c.call("delta-next-page", token), "delta-next-page"); err != nil {
			return nil, err
		}
		return c.nextPage(token)
	}
	cc := c.call("delta", token)
	if err := c.before(cc, "delta"); err != nil {
		return nil, err
	}
	if token != "" && cc.roll(chaosFaultOdds) {
		c.count("delta:410")
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
	c.count(fmt.Sprintf("delta:split-%d", pageCount))

	return c.servePages(pages, page.DeltaLink)
}

func (c *chaosGraph) servePages(pages [][]graph.Item, deltaLink string) *graph.DeltaPage {
	if len(pages) == 1 {
		return &graph.DeltaPage{Items: pages[0], DeltaLink: deltaLink}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pageSeq++
	next := chaosPageTokenPrefix + strconv.Itoa(c.pageSeq)
	c.pages[next] = chaosPendingWindow{pages: pages[1:], deltaLink: deltaLink}

	return &graph.DeltaPage{Items: pages[0], NextLink: next}
}

func (c *chaosGraph) nextPage(token string) (*graph.DeltaPage, error) {
	c.mu.Lock()
	window, ok := c.pages[token]
	c.mu.Unlock()
	if !ok {
		return nil, &graph.GraphError{StatusCode: http.StatusBadRequest, Code: "invalidRequest", Err: graph.ErrBadRequest}
	}

	return c.servePages(window.pages, window.deltaLink), nil
}

// concurrentRemoteWrite is another client overwriting a file on OneDrive
// while the engine is mid-pass: between the observation that planned an
// action and the call that carries it out.
func (r *modelRun) concurrentRemoteWrite(rng *rand.Rand) {
	files := modelEntriesOf(r.remote.tree(), false)
	if len(files) == 0 {
		return
	}
	p := files[rng.IntN(len(files))]
	content := r.nextContent("concurrent")
	r.remote.putFileAt(r.t, p, content)

	r.concurrentMu.Lock()
	defer r.concurrentMu.Unlock()
	r.logf("concurrent remote write %s", p)
	r.concurrentEdits = append(r.concurrentEdits, modelEdit{side: modelRemoteSide, kind: modelOpConcurrentWrite, path: p, content: content})
}

// takeConcurrentEdits returns and clears the edits the chaos layer's other
// client made, so the batch's no-loss check covers them.
func (r *modelRun) takeConcurrentEdits() []modelEdit {
	r.concurrentMu.Lock()
	defer r.concurrentMu.Unlock()
	edits := r.concurrentEdits
	r.concurrentEdits = nil

	return edits
}

// Validates: R-2.3.1, R-6.8.9
//
// The model test through the chaos Graph layer: every batch's first passes
// run in a storm of seeded transient faults, lost responses, interrupted
// sessions, truncated downloads, odd delta windows and concurrent writers;
// once it passes, sync converges with every model check and invariant
// holding. ONEDRIVE_GO_CHAOS_SEEDS widens the search (or name one seed with
// ONEDRIVE_GO_MODEL_SEED).
func TestModel_ChaosGraphConverges(t *testing.T) {
	t.Parallel()

	for _, seed := range chaosSeeds(t) {
		t.Run("seed="+strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()
			opts := modelOptions{chaos: true}
			if raceDetectorEnabled {
				opts.batches = modelRaceBatches
			}
			run := runModelScenarioWith(t, seed, opts)
			require.Positive(t, run.chaos.faultCount(), "the storm injected no fault")
			t.Logf("seed %d faults: %s", seed, run.chaos.faultSummary())
		})
	}
}

// chaosSeeds is the chaos run's seeds: one named seed, a count from the
// environment, or chaosDefaultSeeds (one under -race).
func chaosSeeds(t *testing.T) []uint64 {
	t.Helper()

	if os.Getenv(modelSeedEnv) != "" {
		return modelSeeds(t)
	}
	count := chaosDefaultSeeds
	if raceDetectorEnabled {
		count = 1
	}
	if raw := os.Getenv(chaosSeedEnv); raw != "" {
		parsed, err := strconv.Atoi(raw)
		require.NoError(t, err, chaosSeedEnv)
		count = parsed
	}
	seeds := make([]uint64, count)
	for i := range seeds {
		seeds[i] = uint64(i + 1)
	}

	return seeds
}

// Validates: R-2.15.2
//
// A split window keeps every item and puts each repeated item's last
// occurrence where the unsplit page had it.
func TestChaosGraph_SplitKeepsLastOccurrence(t *testing.T) {
	t.Parallel()

	for seed := uint64(1); seed <= 20; seed++ {
		c := newChaosGraph(seed)
		c.setStorm(true)
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
