package sync

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// What a chaos seed fixes, and the line that reruns one. A seed fixes the
// values a call draws, for a given stream, call number and storm state,
// however the calls of different streams interleave. What the calls meet in
// the fake, which stream a request on a name the fake handed out falls in,
// and which calls a client makes in a step are not the seed's, so the seed
// does not fix the faults a whole run meets (the header of
// chaos_remote_test.go lists each). A failing chaos run logs the command that
// reruns its seed (chaosRerunLine).

const (
	// chaosOrderSeed is the interleaving test's seed. Its storm step splits
	// delta windows and draws faults on their follow-up pages, which the test
	// requires.
	chaosOrderSeed = 1
	// chaosOrderCallsPerStep is how many calls each single-key stream makes
	// in a step.
	chaosOrderCallsPerStep = 8
	// chaosOrderListingsPerStep is how many listings each delta stream makes
	// in a step, each followed to its deltaLink.
	chaosOrderListingsPerStep = 4
	// chaosOrderPageAttempts bounds the attempts at one page of a listing.
	chaosOrderPageAttempts = 6
	// chaosOrderWindowItems is the size of every window the test's fake
	// serves, enough for the layer to split it.
	chaosOrderWindowItems = 9
	// chaosOrderWriteChoices is how many files the test's other client
	// chooses among when it writes: the same count whatever came before.
	chaosOrderWriteChoices = 5
	// chaosOrderOtherWrite is the fault another client's write logs; it
	// fails no call.
	chaosOrderOtherWrite = "fault concurrent-write"
)

// Two chaos layers of one seed, given the same calls with the same step and
// storm state, draw the same for every stream in the stream's own order, and
// every call gets the same answer, whether the streams' calls come one stream
// after another, in reverse, interleaved one call at a time, or from a
// goroutine each. The calls include delta listings that follow a split
// window's pages, so a follow-up page's token, which keys its stream, depends
// on no other stream's traffic. The test's fake answers every call the same
// whatever came before, and its keys are fixed names: the test says nothing
// about a fake whose answers or names depend on earlier calls, nor about
// which calls a client makes.
func TestChaosGraph_StreamDrawsIgnoreHowStreamsInterleave(t *testing.T) {
	t.Parallel()

	wantLog, wantSeen := chaosOrderRun(t.Context(), chaosInTurn)
	require.True(t, slices.ContainsFunc(wantLog, func(d chaosDraw) bool {
		return d.op == chaosOpDeltaNextPage && d.storm &&
			strings.HasPrefix(d.outcome, "fault ") && d.outcome != chaosOrderOtherWrite
	}), "the storm step failed no follow-up page")
	require.True(t, slices.ContainsFunc(wantLog, func(d chaosDraw) bool { return !d.storm }), "no call opened in the calm step")

	for _, order := range []struct {
		name    string
		arrange chaosArrangement
	}{
		{name: "in reverse", arrange: chaosInReverse},
		{name: "interleaved", arrange: chaosInterleaved},
		{name: "a goroutine each", arrange: chaosConcurrently},
	} {
		log, seen := chaosOrderRun(t.Context(), order.arrange)
		assert.Empty(t, firstStreamDifference(chaosStreams(wantLog), chaosStreams(log)), "%s: a stream drew differently", order.name)
		assert.Empty(t, firstStreamDifference(wantSeen, seen), "%s: a call got another answer", order.name)
	}
}

// chaosOrderRun gives a new layer of chaosOrderSeed the test's calls, a storm
// step and then a calm step, each step's calls in the global order arrange
// makes, and returns the layer's fault log and what each stream's calls got.
func chaosOrderRun(ctx context.Context, arrange chaosArrangement) ([]chaosDraw, map[string][]string) {
	c := newChaosGraph(chaosOrderSeed)
	c.concurrentWrite = func(cc *chaosCall) { cc.intN(chaosOrderWriteChoices) }
	client := c.wrap(chaosOrderClient())
	streams := chaosOrderStreams()
	for _, storm := range []bool{true, false} {
		c.startStep(storm)
		steps := make([]func() bool, 0, len(streams))
		for _, stream := range streams {
			steps = append(steps, stream.step(ctx, client))
		}
		arrange(steps)
	}
	seen := make(map[string][]string, len(streams))
	for _, stream := range streams {
		seen[stream.name] = stream.seen
	}

	return c.drawLog(), seen
}

// chaosOrderClient is a fake OneDrive whose every answer depends on the
// request alone, so what a call gets depends on nothing but the layer's draws.
func chaosOrderClient() *engineMockClient {
	return &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, token string) (*graph.DeltaPage, error) {
			items := []graph.Item{{ID: "root", IsRoot: true}}
			for i := range chaosOrderWindowItems {
				items = append(items, graph.Item{ID: fmt.Sprintf("%s-item-%d", token, i)})
			}
			return &graph.DeltaPage{Items: items, DeltaLink: "after-" + token}, nil
		},
		getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			return &graph.Item{ID: itemID}, nil
		},
		downloadFn: func(_ context.Context, _ driveid.ID, itemID string, w io.Writer) (int64, error) {
			n, err := io.WriteString(w, "the content of "+itemID)
			if err != nil {
				return int64(n), fmt.Errorf("write download: %w", err)
			}
			return int64(n), nil
		},
		uploadFn: func(
			_ context.Context, _ driveid.ID, parentID, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc,
		) (*graph.Item, error) {
			return &graph.Item{ID: parentID + "/" + name, Name: name}, nil
		},
	}
}

// chaosOrderStream is one stream of the interleaving test and what its calls
// got. step starts the stream's calls of one step: each call of the function
// it returns makes the stream's next call and reports whether another follows.
type chaosOrderStream struct {
	name string
	step func(ctx context.Context, client *engineMockClient) func() bool
	seen []string
}

// chaosOrderStreams are the interleaving test's streams: three single-key
// streams of different operations, and two delta streams whose listings
// follow their windows' pages.
func chaosOrderStreams() []*chaosOrderStream {
	driveID := driveid.New(engineTestDriveID)

	return []*chaosOrderStream{
		chaosRepeatedCalls("get-item item-a", func(ctx context.Context, client *engineMockClient) string {
			_, err := client.getItemFn(ctx, driveID, "item-a")
			return chaosOutcome(err)
		}),
		chaosRepeatedCalls("download item-b", func(ctx context.Context, client *engineMockClient) string {
			n, err := client.downloadFn(ctx, driveID, "item-b", io.Discard)
			return strconv.FormatInt(n, 10) + " bytes, " + chaosOutcome(err)
		}),
		chaosRepeatedCalls("upload root/c.txt", func(ctx context.Context, client *engineMockClient) string {
			_, err := client.uploadFn(ctx, driveID, "root", "c.txt", strings.NewReader("c"), 1, time.Time{}, nil)
			return chaosOutcome(err)
		}),
		chaosListings(driveID, ""),
		chaosListings(driveID, "seq-5"),
	}
}

// chaosRepeatedCalls is a stream that makes call chaosOrderCallsPerStep times
// a step.
func chaosRepeatedCalls(name string, call func(context.Context, *engineMockClient) string) *chaosOrderStream {
	stream := &chaosOrderStream{name: name}
	stream.step = func(ctx context.Context, client *engineMockClient) func() bool {
		left := chaosOrderCallsPerStep
		return func() bool {
			stream.seen = append(stream.seen, call(ctx, client))
			left--
			return left > 0
		}
	}

	return stream
}

// chaosListings is a delta stream from token: each of its listings follows
// its window's pages to the deltaLink, trying a page that failed again, as
// the remote observer does.
func chaosListings(driveID driveid.ID, token string) *chaosOrderStream {
	stream := &chaosOrderStream{name: "listings from " + strconv.Quote(token)}
	stream.step = func(ctx context.Context, client *engineMockClient) func() bool {
		listings, next, attempts := 0, token, 0
		return func() bool {
			page, err := client.deltaFn(ctx, driveID, next)
			attempts++
			switch {
			case err != nil:
				stream.seen = append(stream.seen, chaosOutcome(err))
				if attempts < chaosOrderPageAttempts {
					return true
				}
			case page.NextLink != "":
				stream.seen = append(stream.seen, fmt.Sprintf("%d items, next %s", len(page.Items), page.NextLink))
				next, attempts = page.NextLink, 0
				return true
			default:
				stream.seen = append(stream.seen, fmt.Sprintf("%d items, deltaLink %s", len(page.Items), page.DeltaLink))
			}
			listings++
			next, attempts = token, 0
			return listings < chaosOrderListingsPerStep
		}
	}

	return stream
}

// chaosOutcome is what a call got, as the interleaving test compares it.
func chaosOutcome(err error) string {
	if err != nil {
		return err.Error()
	}

	return "ok"
}

// chaosArrangement makes the calls of one step's streams in one global order.
// Each stream's own calls keep their order whatever the arrangement.
type chaosArrangement func(steps []func() bool)

// chaosInTurn makes each stream's calls in turn, one stream after another.
func chaosInTurn(steps []func() bool) {
	for _, next := range steps {
		chaosDrain(next)
	}
}

// chaosInReverse is chaosInTurn with the streams in reverse order.
func chaosInReverse(steps []func() bool) {
	for _, next := range slices.Backward(steps) {
		chaosDrain(next)
	}
}

// chaosInterleaved makes one call of each stream in turn until every stream
// has made its calls.
func chaosInterleaved(steps []func() bool) {
	for len(steps) > 0 {
		var more []func() bool
		for _, next := range steps {
			if next() {
				more = append(more, next)
			}
		}
		steps = more
	}
}

// chaosConcurrently makes each stream's calls on a goroutine of its own and
// waits for them all.
func chaosConcurrently(steps []func() bool) {
	var wg stdsync.WaitGroup
	for _, next := range steps {
		wg.Go(func() { chaosDrain(next) })
	}
	wg.Wait()
}

// chaosDrain makes the stream's calls until it has no more in the step.
func chaosDrain(next func() bool) {
	more := true
	for more {
		more = next()
	}
}

// chaosStreams splits a fault log by stream, each stream's entries in the
// order they were logged.
func chaosStreams(log []chaosDraw) map[string][]string {
	streams := map[string][]string{}
	for _, draw := range log {
		streams[draw.stream()] = append(streams[draw.stream()], draw.String())
	}

	return streams
}

// firstStreamDifference is where two sets of streams first part: the first
// stream, by name, whose entries differ from the other set's, and the first of
// its entries that differs; "" when every stream holds the same entries in
// the same order. How the entries of different streams interleave is not
// compared.
func firstStreamDifference(first, second map[string][]string) string {
	names := map[string]struct{}{}
	for name := range first {
		names[name] = struct{}{}
	}
	for name := range second {
		names[name] = struct{}{}
	}
	for _, name := range slices.Sorted(maps.Keys(names)) {
		mine, theirs := first[name], second[name]
		for i := range max(len(mine), len(theirs)) {
			if a, b := chaosEntryAt(mine, i), chaosEntryAt(theirs, i); a != b {
				return fmt.Sprintf("stream %q, entry %d: %q in the first, %q in the second", name, i, a, b)
			}
		}
	}

	return ""
}

// chaosEntryAt is entry i of a stream, or "(none)" past its end.
func chaosEntryAt(entries []string, i int) string {
	if i >= len(entries) {
		return "(none)"
	}

	return entries[i]
}

// chaosRerunPrefix starts the line a failing chaos run logs, so a search of a
// test log finds it.
const chaosRerunPrefix = "seed-replay: "

// chaosRerunPackage is the package path a rerun line names, from the module
// root, and chaosRerunTimeout its go test deadline: a chaos seed alone takes
// minutes on a loaded host.
const (
	chaosRerunPackage = "./internal/sync/"
	chaosRerunTimeout = "30m"
)

// chaosRerunLine is the one command, run from the module root, that reruns the
// chaos seed of test (a test's full name, subtests included): the top-level
// test with the seed named in modelSeedEnv, under the race detector when this
// run had it, which gives the rerun this run's batch count (chaosBatches). It
// reruns the seed, not the run: which requests the client sends in each step
// depends on elapsed time, so the rerun can meet other faults.
func chaosRerunLine(test string, seed uint64) string {
	top, _, _ := strings.Cut(test, "/")
	race := ""
	if raceDetectorEnabled {
		race = " -race"
	}

	return fmt.Sprintf("%s%s=%d go test%s -count=1 -timeout %s -run '^%s$' %s",
		chaosRerunPrefix, modelSeedEnv, seed, race, chaosRerunTimeout, top, chaosRerunPackage)
}

// chaosRerun is what a rerun line names.
type chaosRerun struct {
	test    string
	seed    uint64
	batches int
}

// parseChaosRerunLine reads back a line chaosRerunLine wrote: a go test of
// chaosRerunPackage with the seed in modelSeedEnv.
func parseChaosRerunLine(line string) (chaosRerun, error) {
	command, ok := strings.CutPrefix(line, chaosRerunPrefix)
	if !ok {
		return chaosRerun{}, fmt.Errorf("parse rerun line %q: no %q prefix", line, chaosRerunPrefix)
	}
	fields := strings.Fields(command)
	const leading = 3 // the seed's variable, "go" and "test"
	if len(fields) <= leading || fields[1] != "go" || fields[2] != "test" || fields[len(fields)-1] != chaosRerunPackage {
		return chaosRerun{}, fmt.Errorf("parse rerun line %q: not a go test of %s", line, chaosRerunPackage)
	}
	value, ok := strings.CutPrefix(fields[0], modelSeedEnv+"=")
	if !ok {
		return chaosRerun{}, fmt.Errorf("parse rerun line %q: the seed is not named in %s", line, modelSeedEnv)
	}
	seed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return chaosRerun{}, fmt.Errorf("parse rerun line %q: seed: %w", line, err)
	}
	test, race, err := parseChaosRerunFlags(fields[leading : len(fields)-1])
	if err != nil {
		return chaosRerun{}, fmt.Errorf("parse rerun line %q: %w", line, err)
	}

	return chaosRerun{test: test, seed: seed, batches: chaosBatches(race)}, nil
}

// errChaosRerunNoTest is a rerun line whose flags name no test.
var errChaosRerunNoTest = errors.New("no test named")

// chaosRerunTestFlag is the go test flag that names a rerun line's test.
const chaosRerunTestFlag = "-run"

// parseChaosRerunFlags reads the go test flags of a rerun line: the test -run
// names, and whether the line runs the race detector.
func parseChaosRerunFlags(flags []string) (test string, race bool, err error) {
	for i := 0; i < len(flags); i++ {
		switch flag := flags[i]; flag {
		case "-race":
			race = true
		case "-count=1":
		case "-timeout", chaosRerunTestFlag:
			if i+1 == len(flags) {
				return "", false, fmt.Errorf("%s without a value", flag)
			}
			i++
			if flag == chaosRerunTestFlag {
				test = strings.TrimSuffix(strings.TrimPrefix(flags[i], "'^"), "$'")
			}
		default:
			return "", false, fmt.Errorf("unexpected flag %q", flag)
		}
	}
	if test == "" {
		return "", false, errChaosRerunNoTest
	}

	return test, race, nil
}

// The line a failing chaos run logs is one go test command that reruns its
// seed: parsed back, it names a test this package declares, the run's seed,
// and the run's batch count.
func TestChaosRerunLine_NamesTheTestTheSeedAndTheBatchCount(t *testing.T) {
	t.Parallel()

	const seed = 35
	name := testFuncName(TestModel_ChaosGraphConverges)
	line := chaosRerunLine(name+"/seed="+strconv.Itoa(seed), seed)

	rerun, err := parseChaosRerunLine(line)
	require.NoError(t, err)
	assert.Equal(t, name, rerun.test, line)
	assert.True(t, declaredTestFuncs(t)[rerun.test], "%s: the package declares no test %s", line, rerun.test)
	assert.Equal(t, uint64(seed), rerun.seed, line)
	assert.Equal(t, chaosRunOptions().batchCount(), rerun.batches, line)
}

// A failing chaos run shows one command that reruns it: its failure context
// names the seed and carries no second hint, while a run without the chaos
// layer, which logs no rerun line, keeps the hint.
func TestModelRun_ChaosFailureContextCarriesNoSecondRerunHint(t *testing.T) {
	t.Parallel()

	plain := (&modelRun{seed: 7}).failureContext()
	chaos := (&modelRun{seed: 7, chaos: newChaosGraph(7)}).failureContext()

	assert.Contains(t, plain, modelSeedEnv+"=7")
	assert.NotContains(t, chaos, modelSeedEnv)
	assert.Contains(t, chaos, "seed 7")
}

// declaredTestFuncs is the set of top-level test functions the package's test
// files declare.
func declaredTestFuncs(t *testing.T) map[string]bool {
	t.Helper()

	files, err := filepath.Glob("*_test.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	declared := map[string]bool{}
	for _, file := range files {
		parsed, parseErr := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		require.NoError(t, parseErr, file)
		for _, decl := range parsed.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
				declared[fn.Name.Name] = true
			}
		}
	}

	return declared
}
