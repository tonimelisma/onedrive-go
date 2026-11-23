package sync

import (
	"context"
	"fmt"
	"io"
	"maps"
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
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
)

// What a chaos seed fixes, and the line that reruns one. By construction a
// seed fixes the values a call draws, for a given stream, call number and
// storm state, however the calls of different streams interleave. What the
// calls meet in the fake and which calls a client makes in a step follow the
// run (the header of chaos_remote_test.go lists each). The run waits in
// virtual time and draws every jitter from a seeded source, so a seed usually
// replays the whole run (TestModel_ChaosSameSeedReplaysIdentically); the
// order of goroutines at one virtual instant stays outside the seed. A
// failing chaos run logs the `seed-replay:` line that reruns its seed
// (logSeedReplayOnFailure).

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
func chaosOrderClient() *syncfixture.EngineMockClient {
	return &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, token string) (*graph.DeltaPage, error) {
			items := []graph.Item{{ID: "root", IsRoot: true}}
			for i := range chaosOrderWindowItems {
				items = append(items, graph.Item{ID: fmt.Sprintf("%s-item-%d", token, i)})
			}
			return &graph.DeltaPage{Items: items, DeltaLink: "after-" + token}, nil
		},
		GetItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			return &graph.Item{ID: itemID}, nil
		},
		DownloadFn: func(_ context.Context, _ driveid.ID, itemID string, w io.Writer) (int64, error) {
			n, err := io.WriteString(w, "the content of "+itemID)
			if err != nil {
				return int64(n), fmt.Errorf("write download: %w", err)
			}
			return int64(n), nil
		},
		UploadFn: func(
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
	step func(ctx context.Context, client *syncfixture.EngineMockClient) func() bool
	seen []string
}

// chaosOrderStreams are the interleaving test's streams: three single-key
// streams of different operations, and two delta streams whose listings
// follow their windows' pages.
func chaosOrderStreams() []*chaosOrderStream {
	driveID := driveid.New(syncfixture.EngineTestDriveID)

	return []*chaosOrderStream{
		chaosRepeatedCalls("get-item item-a", func(ctx context.Context, client *syncfixture.EngineMockClient) string {
			_, err := client.GetItemFn(ctx, driveID, "item-a")
			return chaosOutcome(err)
		}),
		chaosRepeatedCalls("download item-b", func(ctx context.Context, client *syncfixture.EngineMockClient) string {
			n, err := client.DownloadFn(ctx, driveID, "item-b", io.Discard)
			return strconv.FormatInt(n, 10) + " bytes, " + chaosOutcome(err)
		}),
		chaosRepeatedCalls("upload root/c.txt", func(ctx context.Context, client *syncfixture.EngineMockClient) string {
			_, err := client.UploadFn(ctx, driveID, "root", "c.txt", strings.NewReader("c"), 1, time.Time{}, nil)
			return chaosOutcome(err)
		}),
		chaosListings(driveID, ""),
		chaosListings(driveID, "seq-5"),
	}
}

// chaosRepeatedCalls is a stream that makes call chaosOrderCallsPerStep times
// a step.
func chaosRepeatedCalls(name string, call func(context.Context, *syncfixture.EngineMockClient) string) *chaosOrderStream {
	stream := &chaosOrderStream{name: name}
	stream.step = func(ctx context.Context, client *syncfixture.EngineMockClient) func() bool {
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
	stream.step = func(ctx context.Context, client *syncfixture.EngineMockClient) func() bool {
		listings, next, attempts := 0, token, 0
		return func() bool {
			page, err := client.DeltaFn(ctx, driveID, next)
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

// chaosReplayRuns is how many times the replay test runs each chaos seed.
const chaosReplayRuns = 20

// Validates: R-6.10.34, R-2.3.1
//
// A chaos seed replays its run, not only its draws: twenty runs of each seed
// the chaos tests use meet the same faults in every stream and dispatch the
// same actions in the same order, and wait the same virtual time to the
// nanosecond. It holds because a one-shot run waits in
// virtual time and every random draw of the run, the retry jitter included,
// comes from a source the seed fixes. The first entry two runs differ in
// names what is left when it fails.
func TestModel_ChaosSameSeedReplaysIdentically(t *testing.T) {
	syncfixture.SkipInShort(t)
	t.Parallel()

	seeds := []uint64{1, 2, chaosRaceSeed}
	opts := chaosRunOptions()
	if syncfixture.RaceDetectorEnabled {
		// The race run is for data races, which one seed exercises; see
		// modelRaceSeeds.
		seeds = []uint64{chaosRaceSeed}
	}
	for _, seed := range seeds {
		t.Run("seed="+strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()

			var streams [chaosReplayRuns]map[string][]string
			var transcripts [chaosReplayRuns][]string
			var waited [chaosReplayRuns]time.Duration
			// The group's Run returns once every parallel run in it ended.
			t.Run("runs", func(t *testing.T) {
				for i := range chaosReplayRuns {
					t.Run("run="+strconv.Itoa(i+1), func(t *testing.T) {
						t.Parallel()
						run := runModelScenarioWith(t, seed, opts)
						streams[i] = chaosStreams(run.chaos.drawLog())
						transcripts[i] = run.transcript
						waited[i] = run.waited
					})
				}
			})

			require.NotEmpty(t, streams[0])
			for i := 1; i < chaosReplayRuns; i++ {
				assert.Empty(t, firstStreamDifference(streams[0], streams[i]), "seed %d: run %d met other faults than run 1", seed, i+1)
				// Every wait of the run is drawn the same, so the run waits
				// the same virtual time to the nanosecond.
				assert.Equal(t, waited[0], waited[i], "seed %d: run %d waited another time than run 1", seed, i+1)
				if !assert.Equal(t, transcripts[0], transcripts[i], "seed %d: run %d dispatched other actions than run 1", seed, i+1) {
					return
				}
			}
		})
	}
}

// chaosEntryAt is entry i of a stream, or "(none)" past its end.
func chaosEntryAt(entries []string, i int) string {
	if i >= len(entries) {
		return "(none)"
	}

	return entries[i]
}

// Validates: R-6.10.25
//
// A failing seed shows one command that reruns it, its `seed-replay:` line:
// the failure context of a model run, with the chaos layer or without it,
// names the seed and carries no second rerun hint. The planner model's run
// is held to the same (TestPlannerModelRun_FailureContextCarriesNoSecondRerunHint).
func TestModelRun_FailureContextCarriesNoSecondRerunHint(t *testing.T) {
	t.Parallel()

	contexts := map[string]string{
		"plain": (&modelRun{seed: 7}).failureContext(),
		"chaos": (&modelRun{seed: 7, chaos: newChaosGraph(7)}).failureContext(),
	}
	for name, context := range contexts {
		assert.Contains(t, context, "seed 7", name)
		assert.NotContains(t, context, "rerun", name)
		assert.NotContains(t, context, "_SEED=", name)
	}
}
