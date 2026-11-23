package sync

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncplan"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Model-based randomized testing in the style of Dropbox's CanopyCheck: each
// batch applies random independent edits to the local tree and to the fake
// OneDrive, syncs until quiet, and checks invariants every correct
// bidirectional sync must hold:
//
//  1. Convergence: the local and remote trees are identical.
//  2. No content loss: every file version written during the batch, on
//     either side, still exists somewhere (an edit beats a delete; a
//     conflict keeps both versions).
//  3. Untouched files are untouched: a file neither side changed, and whose
//     ancestors neither side moved or deleted, keeps its path and content.
//  4. Quiescence: once converged, the next plan is empty.
//
// A failing seed logs its operations and, last, a `seed-replay:` line: the
// command that reruns it, ONEDRIVE_GO_MODEL_SEED=<seed> go test ... (see
// logSeedReplayOnFailure). Widen the search with
// ONEDRIVE_GO_MODEL_SEEDS=<count>, starting at
// ONEDRIVE_GO_MODEL_SEED_START=<seed> (default 1); the crash run reads the
// same variables. A plain run takes the first modelDefaultSeeds; the nightly
// `verify seeds` profile takes a range that starts at a seed derived from the
// date, so every night searches seeds no earlier night ran. A replay holds at
// the commit that failed: each seed that found a bug is kept as a named
// scenario test (engine_move_scenario_test.go), because adding operations
// renumbers what a seed does.
//
// A seed replays exactly (D-090): the engine dispatches each ready frontier in
// an order shuffled by the seed on one worker, and the fake OneDrive shuffles
// its delta pages by the seed, so the order-dependent bugs a pool of workers
// hit only by timing are explored deliberately and reproduced on rerun.
// TestModel_SameSeedReplaysIdentically holds that promise.
//
// Under -race the default drops to modelRaceSeeds. The race detector makes each
// seed 1-2 minutes on a 2-core host (4 seeds measured 204 s), and 12 pushed
// internal/sync past go test's 10-minute package timeout. The race run is there
// to catch data races, which two parallel seeds exercise as well as twelve; seed
// breadth belongs to non-race runs and to ONEDRIVE_GO_MODEL_SEEDS.

const (
	modelBatches = 6
	// modelRaceBatches is the batch count of the replay and chaos runs under
	// -race, where one batch costs about 25 s on a 2-core host.
	modelRaceBatches         = 2
	modelMaxOpsPerSide       = 3
	modelMaxSyncPasses       = 8
	modelDefaultSeeds        = 12
	modelCrashSeedCount      = 4
	modelRaceSeeds           = 2
	modelInitialFolders      = 3
	modelInitialFiles        = 5
	modelSeedEnv             = "ONEDRIVE_GO_MODEL_SEED"
	modelSeedCountEnv        = "ONEDRIVE_GO_MODEL_SEEDS"
	modelSeedStartEnv        = "ONEDRIVE_GO_MODEL_SEED_START"
	modelTraceEnv            = "ONEDRIVE_GO_MODEL_TRACE"
	modelConflictMarker      = ".conflict-"
	modelFileNamePool        = "a.txt b.txt c.md 50%.txt a+b.txt"
	modelFolderNamePool      = "docs photos x q%20r"
	modelOpConcurrentWrite   = "concurrent-write"
	modelReplaceChildName    = "inside.txt"
	modelMirroredMoveOdds    = 3
	modelNotConvergedMessage = "sync did not converge"
	// modelChaosPasses is how many passes of a chaos batch run in a storm.
	modelChaosPasses = 3
)

// Validates: R-2.3.1, R-2.17.1, R-2.17.2, R-2.17.3, R-2.17.6, R-2.17.7, R-2.17.8
func TestRunOnce_RandomizedBidirectionalEditsConverge(t *testing.T) {
	syncfixture.SkipInShort(t)
	t.Parallel()

	for _, seed := range modelSeeds(t) {
		t.Run("seed="+strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()
			syncfixture.LogSeedReplayOnFailure(t, modelSeedEnv, seed)
			runModelScenario(t, seed)
		})
	}
}

// Validates: R-2.3.1
//
// The same seed twice gives the same actions in the same order and the same
// trees (D-090): a failing seed is a reproducible bug, not a flake.
func TestModel_SameSeedReplaysIdentically(t *testing.T) {
	syncfixture.SkipInShort(t)
	t.Parallel()

	seeds := []uint64{3, 28}
	replayBatches := 0
	if syncfixture.RaceDetectorEnabled {
		// Determinism does not depend on the race detector; one seed over
		// fewer batches keeps the race run within go test's package timeout
		// (see modelRaceSeeds).
		seeds, replayBatches = seeds[:1], modelRaceBatches
	}
	for _, seed := range seeds {
		t.Run("seed="+strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()

			var transcripts [2][]string
			for i := range transcripts {
				t.Run("run="+strconv.Itoa(i+1), func(t *testing.T) {
					transcripts[i] = runModelScenarioWith(t, seed, modelOptions{batches: replayBatches}).transcript
				})
			}
			require.NotEmpty(t, transcripts[0])
			assert.Equal(t, transcripts[0], transcripts[1], "two runs of seed %d diverged", seed)
		})
	}
}

// Validates: R-6.5.1, R-6.5.2
//
// Every batch of a few seeds also crashes at a crash point the seed picks
// (D-091), sometimes losing the WAL too, and sync must converge from what the
// crash left with every model check and invariant holding.
func TestModel_CrashAtSeededPointsRecovers(t *testing.T) {
	syncfixture.SkipInShort(t)
	t.Parallel()

	for _, seed := range modelCrashSeeds(t) {
		t.Run("seed="+strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()
			syncfixture.LogSeedReplayOnFailure(t, modelSeedEnv, seed)
			runModelScenarioWith(t, seed, modelOptions{crashes: true})
		})
	}
}

// modelSeedVars are the variables of the engine and crash runs.
func modelSeedVars() syncfixture.SeedVars {
	return syncfixture.SeedVars{Single: modelSeedEnv, Count: modelSeedCountEnv, Start: modelSeedStartEnv}
}

// modelCrashSeeds is the crash run's seeds: the ones the environment names,
// else the first modelCrashSeedCount (one under -race).
func modelCrashSeeds(t *testing.T) []uint64 {
	t.Helper()

	count := modelCrashSeedCount
	if syncfixture.RaceDetectorEnabled {
		// The race run is for data races, which one seed exercises; see
		// modelRaceSeeds.
		count = 1
	}

	return syncfixture.SeedsFromEnv(t, modelSeedVars(), count)
}

// modelSeeds is the engine run's seeds: the ones the environment names, else
// the first modelDefaultSeeds (modelRaceSeeds under -race).
func modelSeeds(t *testing.T) []uint64 {
	t.Helper()

	count := modelDefaultSeeds
	if syncfixture.RaceDetectorEnabled {
		count = modelRaceSeeds
	}

	return syncfixture.SeedsFromEnv(t, modelSeedVars(), count)
}

type modelRun struct {
	t        *testing.T
	seed     uint64
	rng      *rand.Rand
	remote   *moveScenarioRemote
	eng      *testEngine
	syncRoot string
	harness  *syncHarness
	// logMu guards writes and log: in watch mode the chaos layer's other
	// client writes and logs from the engine's goroutines while the model
	// does from its own.
	logMu  stdsync.Mutex
	writes int
	log    []string

	// caseSensitive gates the operations only a case-sensitive local
	// filesystem can express.
	caseSensitive bool
	// expectHoldOf is the side a wipe emptied: the next pass must hold the
	// deletes it would propagate.
	expectHoldOf string
	// wipeDeletesUnheld counts the local deletes passes ran without a hold
	// while a chaos run saw a remote wipe in part (requireWipeHeld).
	wipeDeletesUnheld int
	// afterFirstPass is a user's response to what the first pass did (a
	// collision to rename); its edits join the batch's.
	afterFirstPass func() []syncfixture.ModelEdit
	// clock is the engine's clock, a passage of time ahead of the wall clock
	// that grows by modelPassInterval before every pass after a batch's
	// first.
	clock *modelClock

	// chaos, when set, is the chaos Graph layer between the engine and the
	// fake; concurrentEdits are the edits its other client made.
	chaos           *chaosGraph
	concurrentMu    stdsync.Mutex
	concurrentEdits []syncfixture.ModelEdit

	// transcript is every dispatched action and every converged tree, in
	// order, with conflict-copy timestamps masked: two runs of one seed must
	// produce the same transcript.
	transcriptMu stdsync.Mutex
	transcript   []string

	// noWipe keeps applyBatchEdits from wiping a side: a watch cannot
	// confirm the held deletes a wipe needs.
	noWipe bool
	// dispatched is signaled, without blocking, at every dispatch.
	dispatched chan struct{}
	// faults is the run's seeded fault plan in watch mode, and newFiles
	// numbers the files its second waves create.
	faults   watchFaultPlan
	newFiles int
	// waited is how long a run in virtual time waited on its bubble's
	// clock (runModelSeedInVirtualTime); zero for a watch run.
	waited time.Duration
}

func runModelScenario(t *testing.T, seed uint64) *modelRun {
	t.Helper()

	return runModelScenarioWith(t, seed, modelOptions{})
}

// modelOptions are the stresses a model run adds to its random edits.
type modelOptions struct {
	// crashes makes every batch crash at a crash point chosen by the seed
	// (D-091): sync continues on a fresh engine opened on what the crash
	// left, and the batch's checks apply to it.
	crashes bool
	// chaos puts the chaos Graph layer (D-092) between the engine and the
	// fake OneDrive: the first modelChaosPasses passes of every batch run in
	// a storm of seeded faults, the rest in calm.
	chaos bool
	// batches overrides modelBatches; zero keeps it.
	batches int
	// watch syncs every batch in watch mode instead of one-shot passes
	// (watchBatch): the edits arrive while the watch runs, and the batch
	// ends when the watch alone has converged.
	watch bool
}

// batchCount is how many batches a run under o makes.
func (o modelOptions) batchCount() int {
	if o.batches > 0 {
		return o.batches
	}

	return modelBatches
}

// runModelScenarioWith runs one seed under opts. A one-shot run runs in
// virtual time (runModelSeedInVirtualTime); a watch run, whose local
// observer waits on the kernel's file events, runs on the wall clock with
// its long waits compressed (modelClock.compressed).
func runModelScenarioWith(t *testing.T, seed uint64, opts modelOptions) *modelRun {
	t.Helper()

	report := newModelSeedReport(t, seed)
	if opts.watch {
		return runModelSeed(t, seed, opts, report)
	}

	return runModelSeedInVirtualTime(t, seed, opts, report)
}

// runModelSeed runs one seed under opts on t, telling report about the chaos
// layer it builds.
func runModelSeed(t *testing.T, seed uint64, opts modelOptions, report *modelSeedReport) *modelRun {
	t.Helper()

	crashes := opts.crashes
	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.shuffleDelta(seed)
	client := remote.client(t)
	var chaos *chaosGraph
	if opts.chaos {
		chaos = newChaosGraph(seed)
		client = chaos.wrap(client)
		report.setChaos(chaos)
	}
	h := newSyncHarnessWithClient(t, remote, client)
	run := &modelRun{
		t: t, seed: seed, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), //nolint:gosec // deterministic test RNG
		remote: remote, syncRoot: h.syncRoot, harness: h,
		caseSensitive: localFSIsCaseSensitive(t.TempDir()),
		clock:         newModelClock(seed),
		chaos:         chaos,
		noWipe:        opts.watch,
		dispatched:    make(chan struct{}, 1),
	}
	if opts.watch {
		run.faults = newWatchFaultPlan(seed, opts.batchCount(), chaos != nil)
	}
	if chaos != nil {
		chaos.concurrentWrite = run.concurrentRemoteWrite
		chaos.churnPlaceholder = func(itemID string) { run.remote.churnETag(itemID) }
	}
	run.adoptEngine(h.eng)
	crashRng := rand.New(rand.NewPCG(seed^0xc2b2ae3d27d4eb4f, seed)) //nolint:gosec // deterministic test RNG

	run.seedInitialTree()
	initialEdits := run.syncUntilQuiet("initial")
	initial := run.requireConverged("initial")
	run.requireWrittenContentSurvives("initial", initialEdits, initial)

	for batch := 1; batch <= opts.batchCount(); batch++ {
		label := fmt.Sprintf("batch %d", batch)
		before := run.localTree()
		h.beginRound(t)
		var edits []syncfixture.ModelEdit
		if opts.watch {
			edits = run.watchBatch(label, run.faults.storm(batch))
		} else {
			edits = run.applyBatchEdits(label)
			var capture *crashCapture
			if crashes {
				capture = run.armCrash(crashRng)
			}
			edits = append(edits, run.syncUntilQuiet(label)...)
			if capture != nil && capture.captured {
				h = run.recoverFrom(capture, label)
				edits = append(edits, run.syncUntilQuiet(label+" after the crash")...)
			}
		}
		converged := run.requireConverged(label)
		run.requireWrittenContentSurvives(label, edits, converged)
		run.requireUntouchedFilesUnchanged(label, before, edits, converged)
		if !assertSyncInvariants(t, h) {
			require.FailNow(t, "sync invariants violated", "%s: %s\nstate:%s", label, run.failureContext(), syncStateDump(t, run.eng))
		}
		run.recordf("%s converged: %v", label, modelPaths(converged))
	}

	return run
}

// adoptEngine makes eng the model's engine: the model's clock, one worker,
// the seed's dispatch order, and a record of every publication and dispatch.
func (r *modelRun) adoptEngine(eng *testEngine) {
	r.eng = eng
	eng.hooks.clock = r.clock
	order := eng.hooks.dispatchOrder
	if order == nil {
		order = dispatchOrderShuffle(r.seed)
	}
	eng.hooks.serializeWorkers = true
	// One hash worker: the bubble fixes time, not the order in which
	// goroutines that run at one instant reach a shared step, and a scan's
	// parallel hash workers are such goroutines.
	eng.checkWorkers = 1
	eng.hooks.shadowPlanned = r.checkShadowPlan
	eng.hooks.dispatchOrder = order
	eng.hooks.recordFrontier = r.recordFrontierStep
}

// checkShadowPlan is every pass's comparison with the identity-keyed shadow
// plan: each difference is logged with its reason, and one no known reason
// covers fails the seed. It runs on the engine's goroutine, so it reports
// through the goroutine-safe t.Logf and assert.Failf only.
func (r *modelRun) checkShadowPlan(comparison shadowPlanComparison) {
	if comparison.err != nil {
		assert.Failf(r.t, "shadow planner failed", "seed %d: the identity-keyed shadow planner failed: %v", r.seed, comparison.err)
		return
	}
	lines := make([]string, 0, len(comparison.differences))
	for i := range comparison.differences {
		lines = append(lines, comparison.differences[i].String())
	}
	if len(lines) > 0 {
		r.t.Logf("seed %d shadow plan differences:\n  %s", r.seed, strings.Join(lines, "\n  "))
	}
	if comparison.unexplained > 0 {
		assert.Failf(r.t, "unexplained shadow plan differences", "seed %d: %d shadow plan differences have no known reason:\n  %s",
			r.seed, comparison.unexplained, strings.Join(lines, "\n  "))
	}
}

// armCrash installs a capture at a crash point the seed picks, half the time
// with the WAL lost (everything since a checkpoint taken now is lost).
func (r *modelRun) armCrash(rng *rand.Rand) *crashCapture {
	points := syncfixture.AllCrashPoints()
	capture := &crashCapture{
		point:   points[rng.IntN(len(points))],
		lostWAL: rng.IntN(2) == 0,
		srcDir:  filepath.Dir(r.syncRoot),
		dstDir:  r.t.TempDir(),
		remote:  r.remote,
	}
	if capture.lostWAL {
		_, err := r.eng.baseline.RawDBForTest().ExecContext(r.t.Context(), `PRAGMA wal_checkpoint(TRUNCATE)`)
		require.NoError(r.t, err)
	}
	r.eng.armCrash(capture.hook)

	return capture
}

// recoverFrom continues the run on a fresh engine opened on what the crash
// left behind.
func (r *modelRun) recoverFrom(capture *crashCapture, label string) *syncHarness {
	kind := "crash"
	if capture.lostWAL {
		kind = "crash with the WAL lost"
	}
	r.logf("%s: %s at %s; a fresh engine opens what it left", label, kind, capture.point)
	held := r.harness.held
	recovered := capture.reopen(r.t)
	recovered.held = held
	// An item an earlier lost WAL left without its intent stays allowed while
	// it is still empty: by now it is synced, so reopen, which names only
	// items the snapshot's database does not know, does not name it again.
	for _, itemID := range recovered.remote.emptyFiles() {
		if r.harness.placeholdersWithoutIntent[itemID] {
			recovered.placeholdersWithoutIntent[itemID] = true
		}
	}
	r.harness = recovered
	r.remote = recovered.remote
	r.syncRoot = recovered.syncRoot
	r.adoptEngine(recovered.eng)

	return recovered
}

// modelConflictStamp is the timestamp in a conflict copy's name, which comes
// from the wall clock and so differs between two runs of one seed.
var modelConflictStamp = regexp.MustCompile(`\.conflict-\d{8}-\d{6}`)

func (r *modelRun) recordf(format string, args ...any) {
	line := modelConflictStamp.ReplaceAllString(fmt.Sprintf(format, args...), ".conflict-STAMP")
	r.transcriptMu.Lock()
	defer r.transcriptMu.Unlock()
	r.transcript = append(r.transcript, line)
}

// recordFrontierStep keeps every action a pass publishes or dispatches in
// the transcript, so a failing seed's kept log shows each pass, one with a
// single action or only publications included.
func (r *modelRun) recordFrontierStep(step frontierStep, action *syncdomain.Action) {
	r.recordf("%s %s %s <- %s", step, action.Type, action.Path, action.OldPath)
	if step != frontierDispatched {
		return
	}
	select {
	case r.dispatched <- struct{}{}:
	default:
	}
}

func (r *modelRun) logf(format string, args ...any) {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	r.log = append(r.log, fmt.Sprintf(format, args...))
}

// failureContext names the seed and the operations of the run. The
// `seed-replay:` line a failing seed logs (logSeedReplayOnFailure) is the one
// command that reruns it, so the context carries no second one.
func (r *modelRun) failureContext() string {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	return fmt.Sprintf("seed %d; operations:\n  %s", r.seed, strings.Join(r.log, "\n  "))
}

func (r *modelRun) nextContent(side string) string {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	r.writes++
	return fmt.Sprintf("seed %d %s write %d\n", r.seed, side, r.writes)
}

// seedInitialTree builds a random starting tree on OneDrive.
func (r *modelRun) seedInitialTree() {
	for range modelInitialFolders {
		if folder := r.newPathIn(r.remoteFolders(), modelFolderNamePool, r.remoteTreeView()); folder != "" {
			r.remote.mkdirAt(r.t, folder)
			r.logf("setup remote mkdir %s", folder)
		}
	}
	for range modelInitialFiles {
		if file := r.newPathIn(r.remoteFolders(), modelFileNamePool, r.remoteTreeView()); file != "" {
			r.remote.putFileAt(r.t, file, r.nextContent(syncfixture.ModelRemoteSide))
			r.logf("setup remote put %s", file)
		}
	}
}

// syncUntilQuiet runs passes until one transfers and changes nothing. Early
// passes may report failures (a move racing a delete is retried); the last
// one must be clean.
//
// The model's trees are small, so random deletes often empty a side and trip
// the delete-safety hold. The model plays a user who confirms: a pass that
// held deletes is followed by a `--confirm-deletes` pass, and the held pass
// itself counts as a change so the loop keeps going.
func (r *modelRun) syncUntilQuiet(label string) []syncfixture.ModelEdit {
	confirm := false
	var userEdits []syncfixture.ModelEdit
	stormPasses := 0
	if r.chaos != nil {
		stormPasses = modelChaosPasses
	}
	checkWipe := true
	for pass := 1; pass <= modelMaxSyncPasses+stormPasses; pass++ {
		storm := r.startPass(pass, stormPasses)
		report, err := r.eng.RunOnce(r.t.Context(), syncdomain.SyncBidirectional, RunOptions{ConfirmDeletes: confirm})
		if storm && err != nil {
			// A fault the storm injected into observation fails the pass
			// before it plans; the next pass starts over.
			r.logf("%s pass %d (storm): %v", label, pass, err)
			userEdits = append(userEdits, r.takeConcurrentEdits()...)
			continue
		}
		require.NoError(r.t, err, "%s pass %d: %s", label, pass, r.failureContext())
		// Another client's writes during the pass come before the user's
		// response to it.
		userEdits = append(userEdits, r.takeConcurrentEdits()...)
		held := report.HeldLocalDeletes + report.HeldRemoteDeletes
		confirm = held > 0
		changed := modelPassChanges(report)
		if storm {
			// Only a calm pass may end the loop.
			changed++
		}
		if checkWipe {
			// The first pass that observed both sides must hold a wipe; in a
			// storm, the first that observed the wipe whole (requireWipeHeld).
			checkWipe = !r.requireWipeHeld(label, report)
		}
		if r.afterFirstPass != nil {
			userEdits = append(userEdits, r.afterFirstPass()...)
			r.afterFirstPass = nil
			changed++
		}
		r.tracePass(label, pass, report)
		// The user renames what the engine reports as a name collision,
		// once the pass is quiet or the collision is what failed.
		quiet := changed == 0 && report.Failed == 0
		if quiet || (report.Failed > 0 && pass > 1) {
			resolved := r.resolveNameCollisions()
			if quiet && len(resolved) == 0 {
				// A wipe a storm let through in part, with nothing left
				// to hold, ends its check with the loop.
				r.expectHoldOf, r.wipeDeletesUnheld = "", 0
				return userEdits
			}
			userEdits = append(userEdits, resolved...)
		}
	}
	require.Failf(r.t, modelNotConvergedMessage, "%s: still changing after %d sync runs (%d in a storm); %s\nstate:%s",
		label, modelMaxSyncPasses+stormPasses, stormPasses, r.failureContext(), syncStateDump(r.t, r.eng))

	return userEdits
}

// startPass moves the clock to the pass and sets the chaos layer's storm,
// reporting whether this pass runs in one.
func (r *modelRun) startPass(pass, stormPasses int) bool {
	if pass > 1 {
		r.clock.advance(modelPassInterval)
	}
	storm := pass <= stormPasses
	if r.chaos != nil {
		r.chaos.startStep(storm)
	}

	return storm
}

// modelPassChanges is how much a pass changed. A baseline update can be half
// of a change: the same rename on both sides is rekeyed in one pass and its
// content reconciled in the next, so a pass that only updated the baseline
// is not quiet. Held deletes count too: the next pass confirms them.
func modelPassChanges(report *Report) int {
	return report.Uploads + report.Downloads + report.LocalDeletes + report.RemoteDeletes +
		report.FolderCreates + report.Moves + report.BaselineUpdates + report.HeldLocalDeletes + report.HeldRemoteDeletes
}

func (r *modelRun) tracePass(label string, pass int, report *Report) {
	if os.Getenv(modelTraceEnv) == "" {
		return
	}
	r.t.Logf("%s pass %d: up=%d down=%d ldel=%d rdel=%d mkdir=%d mv=%d ok=%d failed=%d errors=%v\n  local:  %v\n  remote: %v",
		label, pass, report.Uploads, report.Downloads, report.LocalDeletes, report.RemoteDeletes,
		report.FolderCreates, report.Moves, report.Succeeded, report.Failed, report.Errors,
		modelPaths(r.localTree()), modelPaths(r.remote.tree()))
}

// modelPassInterval is how much later the model's user runs each further
// pass of a batch: past the longest retry backoff (one hour), so work an
// earlier pass failed is due again, as it would be by the time a user, or
// watch mode, syncs next. Without it a retry held by its backoff read as
// non-convergence after eight instant passes.
const modelPassInterval = time.Hour + time.Minute

// modelClock is a fake clock: the instant the run began plus an offset that
// moves only when the model advances it or a wait the engine armed on it
// ends, so time stands still between events and a seed replays the same
// times. A timer or sleep still waits its real delay, then moves the clock
// to the instant it was due, so a wait that re-reads the clock sees it pass.
// Its jitter is drawn from the seed.
type modelClock struct {
	syncdomain.RealClock
	mu     stdsync.Mutex
	base   time.Time
	offset time.Duration
	jitter *rand.Rand
	// compressed, while set, makes a wait between modelWatchCompressFrom and
	// modelWatchCompressUpTo end after modelWatchCompressedWait of real
	// time, the clock still moving to the instant it was due: a watch's
	// retry backoff passes as it would over a long watch, in milliseconds.
	compressed atomic.Bool
}

// realDelay is how long a wait of delay takes in real time.
func (c *modelClock) realDelay(delay time.Duration) time.Duration {
	if c.compressed.Load() && delay >= modelWatchCompressFrom && delay <= modelWatchCompressUpTo {
		return modelWatchCompressedWait
	}

	return delay
}

func newModelClock(seed uint64) *modelClock {
	return &modelClock{
		base:   time.Now(),
		jitter: rand.New(rand.NewPCG(seed, seed^0x6a09e667f3bcc909)), //nolint:gosec // deterministic test RNG
	}
}

func (c *modelClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.base.Add(c.offset)
}

// Jitter draws from the seed's own stream, so retry jitter replays.
func (c *modelClock) Jitter(maxDelay time.Duration) time.Duration {
	if maxDelay <= 0 {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	return time.Duration(c.jitter.Int64N(int64(maxDelay)))
}

// AfterFunc runs fn after delay, with the clock moved to the instant it was
// due.
func (c *modelClock) AfterFunc(delay time.Duration, fn func()) syncdomain.SyncTimer {
	due := c.Now().Add(delay)

	return c.RealClock.AfterFunc(c.realDelay(delay), func() {
		c.reach(due)
		fn()
	})
}

// Sleep waits delay and moves the clock to the instant it ended.
func (c *modelClock) Sleep(ctx context.Context, delay time.Duration) error {
	due := c.Now().Add(delay)
	if err := c.RealClock.Sleep(ctx, c.realDelay(delay)); err != nil {
		return err //nolint:wrapcheck // the real clock's own error
	}
	c.reach(due)

	return nil
}

// reach moves the clock forward to t, never back.
func (c *modelClock) reach(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now := c.base.Add(c.offset); now.Before(t) {
		c.offset += t.Sub(now)
	}
}

func (c *modelClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// requireWipeHeld checks that the first pass after a side was wiped held the
// deletes instead of propagating them (D-002), and reports whether the check
// is done. The hold for a vanished side (side_vanished) applies to a pass
// whose observation sees the side empty. Through the chaos layer a pass can
// see the wipe in part: a storm leaves items OneDrive no longer lists that
// it could not confirm gone ("could not confirm an item missing from a
// listing is gone; keeping it"), or a delta that has not shown the deletes
// yet. Such a pass may run the deletes it did see, as few as the
// proportional rule lets through without a hold (syncplan.DeleteSafetyMinItems
// in all), and the check moves to the next pass, until one holds the rest or
// nothing of the wipe is left to delete. A pass that resynced after a 410
// uploads what OneDrive no longer has instead of deleting it here (R-2.15.3),
// which loses nothing either. A local wipe is seen whole, but the pass that
// held it can fail in the storm, so the hold is not confirmed, and a later
// pass finds the local side no longer empty once a download brought back a
// file OneDrive edited (an edit beats a delete): it may run the few OneDrive
// deletes left, as the proportional rule lets through (chaos seed 75).
func (r *modelRun) requireWipeHeld(label string, report *Report) bool {
	switch r.expectHoldOf {
	case syncfixture.ModelLocalSide:
		if r.chaos != nil && report.HeldRemoteDeletes == 0 {
			r.wipeDeletesUnheld += report.RemoteDeletes
			require.Less(r.t, r.wipeDeletesUnheld, syncplan.DeleteSafetyMinItems,
				"%s: a wiped local side ran more OneDrive deletes without a hold than the proportional rule lets through; %s", label, r.failureContext())
			if report.Downloads == 0 && len(r.localTree()) == 0 && len(r.remote.tree()) > 0 {
				// The pass that held the wipe failed in the storm and the
				// hold was not confirmed: the next pass holds it again.
				return false
			}
			break
		}
		require.Positive(r.t, report.HeldRemoteDeletes, "%s: a wiped local side must hold the OneDrive deletes; %s", label, r.failureContext())
	case syncfixture.ModelRemoteSide:
		if r.chaos != nil && report.HeldLocalDeletes == 0 {
			r.wipeDeletesUnheld += report.LocalDeletes
			require.Less(r.t, r.wipeDeletesUnheld, syncplan.DeleteSafetyMinItems,
				"%s: a wiped OneDrive ran more local deletes without a hold than the proportional rule lets through; %s", label, r.failureContext())
			if report.Uploads == 0 && len(r.localTree()) > 0 {
				// The wipe was seen in part or not yet: the next pass, which
				// sees more of it, holds what is left.
				return false
			}
			break
		}
		require.Positive(r.t, report.HeldLocalDeletes, "%s: a wiped OneDrive must hold the local deletes; %s", label, r.failureContext())
	}
	r.expectHoldOf = ""
	r.wipeDeletesUnheld = 0

	return true
}

func (r *modelRun) requireConverged(label string) map[string]string {
	local := r.localTree()
	remote := r.remote.tree()
	if !assert.Equal(r.t, remote, local, "%s: local and OneDrive differ; %s", label, r.failureContext()) {
		require.FailNow(r.t, modelNotConvergedMessage, "state:%s", syncStateDump(r.t, r.eng))
	}

	return local
}

// requireWrittenContentSurvives checks invariant 2: content a side wrote and
// still held when the batch ended exists somewhere after sync.
func (r *modelRun) requireWrittenContentSurvives(label string, edits []syncfixture.ModelEdit, converged map[string]string) {
	present := make(map[string]bool, len(converged))
	for _, content := range converged {
		present[content] = true
	}
	var retained map[string]bool
	for i, edit := range edits {
		if edit.Content == "" || syncfixture.ModelOverwrittenLater(edits, i) {
			continue
		}
		if !present[edit.Content] && r.chaos != nil {
			// Another client writing mid-pass (to this item, or to the item
			// after a move) supersedes content on OneDrive, which keeps it
			// as a version: kept, not lost. Without the chaos layer nobody
			// else writes, and the converged trees must hold it.
			if retained == nil {
				retained = r.retainedContents()
			}
			if retained[edit.Content] {
				continue
			}
		}
		assert.True(r.t, present[edit.Content],
			"%s: %s %s of %s was lost; %s", label, edit.Side, edit.Kind, edit.Path, r.failureContext())
	}
}

// retainedContents is every content kept outside both trees: OneDrive's
// recycle bin and version history, and the local trash.
func (r *modelRun) retainedContents() map[string]bool {
	retained := map[string]bool{}
	for _, content := range r.remote.retainedContents() {
		retained[content] = true
	}
	for _, content := range harnessTrashContents(r.t, r.eng) {
		retained[content] = true
	}

	return retained
}

// requireUntouchedFilesUnchanged checks invariant 3.
func (r *modelRun) requireUntouchedFilesUnchanged(
	label string,
	before map[string]string,
	edits []syncfixture.ModelEdit,
	converged map[string]string,
) {
	for p, content := range before {
		if content == syncfixture.ModelFolderSentinel {
			continue
		}
		touched := false
		for _, edit := range edits {
			if edit.Touches(p) {
				touched = true
				break
			}
		}
		if touched {
			continue
		}
		assert.Equal(r.t, content, converged[p], "%s: untouched %s changed; %s", label, p, r.failureContext())
	}
}

// localTree is the sync root as path -> content, folders as "/", ignoring
// the engine's own transfer artifacts.
func (r *modelRun) localTree() map[string]string {
	out, err := readModelLocalTree(r.syncRoot)
	require.NoError(r.t, err)

	return out
}

// readModelLocalTree reads the folder at syncRoot as path -> content,
// folders as the model's folder sentinel.
func readModelLocalTree(syncRoot string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(syncRoot, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(syncRoot, p)
		if relErr != nil {
			return fmt.Errorf("relative path of %s: %w", p, relErr)
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			out[rel] = syncfixture.ModelFolderSentinel
			return nil
		}
		data, readErr := os.ReadFile(p) //nolint:gosec // test reads its own temp tree
		if readErr != nil {
			return fmt.Errorf("read %s: %w", p, readErr)
		}
		out[rel] = string(data)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read local tree %s: %w", syncRoot, err)
	}

	return out, nil
}

func (r *modelRun) remoteTreeView() map[string]string { return r.remote.tree() }

func (r *modelRun) remoteFolders() []string { return modelFoldersOf(r.remote.tree()) }

func (r *modelRun) localFolders() []string { return modelFoldersOf(r.localTree()) }

func modelFoldersOf(tree map[string]string) []string {
	folders := []string{""}
	for p, content := range tree {
		if content == syncfixture.ModelFolderSentinel {
			folders = append(folders, p)
		}
	}
	sort.Strings(folders)

	return folders
}

func modelEntriesOf(tree map[string]string, wantFolders bool) []string {
	var entries []string
	for p, content := range tree {
		if (content == syncfixture.ModelFolderSentinel) == wantFolders {
			entries = append(entries, p)
		}
	}
	sort.Strings(entries)

	return entries
}

func (r *modelRun) pick(options []string) string {
	if len(options) == 0 {
		return ""
	}

	return options[r.rng.IntN(len(options))]
}

// newPathIn picks a random folder and a pool name not yet taken there. Names
// are compared case-insensitively, as OneDrive does.
func (r *modelRun) newPathIn(folders []string, pool string, tree map[string]string) string {
	folder := r.pick(folders)
	name := r.pick(strings.Fields(pool))
	candidate := path.Join(folder, name)
	for p := range tree {
		if strings.EqualFold(p, candidate) {
			return ""
		}
	}

	return candidate
}

// localBasicEdit is one of the original local operations: create, edit,
// delete, move, mkdir or a type change.
func (r *modelRun) localBasicEdit(kind string, tree map[string]string) (syncfixture.ModelEdit, bool) {
	switch kind {
	case syncfixture.ModelOpReplace:
		return r.localReplace(tree)
	case syncfixture.ModelOpCreateFile:
		p := r.newPathIn(r.localFolders(), modelFileNamePool, tree)
		if p == "" {
			return syncfixture.ModelEdit{}, false
		}
		content := r.nextContent(syncfixture.ModelLocalSide)
		syncfixture.WriteLocalFile(r.t, r.syncRoot, filepath.FromSlash(p), content)
		r.logf("local create %s", p)
		return syncfixture.ModelEdit{Side: syncfixture.ModelLocalSide, Kind: syncfixture.ModelOpCreateFile, Path: p, Content: content}, true
	case syncfixture.ModelOpDelete:
		p := r.pick(append(modelEntriesOf(tree, false), modelEntriesOf(tree, true)...))
		if p == "" {
			return syncfixture.ModelEdit{}, false
		}
		require.NoError(r.t, os.RemoveAll(filepath.Join(r.syncRoot, filepath.FromSlash(p))))
		r.logf("local delete %s", p)
		return syncfixture.ModelEdit{Side: syncfixture.ModelLocalSide, Kind: syncfixture.ModelOpDelete, Path: p}, true
	case syncfixture.ModelOpMove:
		return r.localMove(tree)
	default:
		p := r.newPathIn(r.localFolders(), modelFolderNamePool, tree)
		if p == "" {
			return syncfixture.ModelEdit{}, false
		}
		require.NoError(r.t, os.MkdirAll(filepath.Join(r.syncRoot, filepath.FromSlash(p)), 0o750))
		r.logf("local mkdir %s", p)
		return syncfixture.ModelEdit{Side: syncfixture.ModelLocalSide, Kind: syncfixture.ModelOpMakeFolder, Path: p}, true
	}
}

func (r *modelRun) localMove(tree map[string]string) (syncfixture.ModelEdit, bool) {
	from, to, ok := r.pickMove(tree)
	if !ok {
		return syncfixture.ModelEdit{}, false
	}
	require.NoError(r.t, os.Rename(
		filepath.Join(r.syncRoot, filepath.FromSlash(from)),
		filepath.Join(r.syncRoot, filepath.FromSlash(to)),
	))
	r.logf("local move %s -> %s", from, to)

	return syncfixture.ModelEdit{Side: syncfixture.ModelLocalSide, Kind: syncfixture.ModelOpMove, Path: from, To: to}, true
}

// remoteBasicEdit is one of the original remote operations.
func (r *modelRun) remoteBasicEdit(kind string, tree map[string]string) (syncfixture.ModelEdit, bool) {
	switch kind {
	case syncfixture.ModelOpReplace:
		return r.remoteReplace(tree)
	case syncfixture.ModelOpCreateFile:
		p := r.newPathIn(modelFoldersOf(tree), modelFileNamePool, tree)
		if p == "" {
			return syncfixture.ModelEdit{}, false
		}
		content := r.nextContent(syncfixture.ModelRemoteSide)
		r.remote.putFileAt(r.t, p, content)
		r.logf("remote create %s", p)
		return syncfixture.ModelEdit{Side: syncfixture.ModelRemoteSide, Kind: syncfixture.ModelOpCreateFile, Path: p, Content: content}, true
	case syncfixture.ModelOpEditFile:
		return r.remoteEditFile(tree)
	case syncfixture.ModelOpDelete:
		p := r.pick(append(modelEntriesOf(tree, false), modelEntriesOf(tree, true)...))
		if p == "" {
			return syncfixture.ModelEdit{}, false
		}
		r.remote.deleteAt(r.t, p)
		r.logf("remote delete %s", p)
		return syncfixture.ModelEdit{Side: syncfixture.ModelRemoteSide, Kind: syncfixture.ModelOpDelete, Path: p}, true
	case syncfixture.ModelOpMove:
		from, to, ok := r.pickMove(tree)
		if !ok {
			return syncfixture.ModelEdit{}, false
		}
		r.remote.moveAt(r.t, from, to)
		r.logf("remote move %s -> %s", from, to)
		return syncfixture.ModelEdit{Side: syncfixture.ModelRemoteSide, Kind: syncfixture.ModelOpMove, Path: from, To: to}, true
	default:
		p := r.newPathIn(modelFoldersOf(tree), modelFolderNamePool, tree)
		if p == "" {
			return syncfixture.ModelEdit{}, false
		}
		r.remote.mkdirAt(r.t, p)
		r.logf("remote mkdir %s", p)
		return syncfixture.ModelEdit{Side: syncfixture.ModelRemoteSide, Kind: syncfixture.ModelOpMakeFolder, Path: p}, true
	}
}

// localReplace changes an entry's type in place: a file becomes a folder with
// one new file in it, a folder (with its subtree) becomes a file.
func (r *modelRun) localReplace(tree map[string]string) (syncfixture.ModelEdit, bool) {
	p := r.pick(append(modelEntriesOf(tree, false), modelEntriesOf(tree, true)...))
	if p == "" {
		return syncfixture.ModelEdit{}, false
	}
	abs := filepath.Join(r.syncRoot, filepath.FromSlash(p))
	require.NoError(r.t, os.RemoveAll(abs))
	content := r.nextContent(syncfixture.ModelLocalSide)
	if tree[p] == syncfixture.ModelFolderSentinel {
		syncfixture.WriteLocalFile(r.t, r.syncRoot, filepath.FromSlash(p), content)
	} else {
		syncfixture.WriteLocalFile(r.t, r.syncRoot, filepath.FromSlash(path.Join(p, modelReplaceChildName)), content)
	}
	r.logf("local replace %s", p)

	return syncfixture.ModelEdit{Side: syncfixture.ModelLocalSide, Kind: syncfixture.ModelOpReplace, Path: p, Content: content}, true
}

func (r *modelRun) remoteReplace(tree map[string]string) (syncfixture.ModelEdit, bool) {
	p := r.pick(append(modelEntriesOf(tree, false), modelEntriesOf(tree, true)...))
	if p == "" {
		return syncfixture.ModelEdit{}, false
	}
	r.remote.deleteAt(r.t, p)
	content := r.nextContent(syncfixture.ModelRemoteSide)
	if tree[p] == syncfixture.ModelFolderSentinel {
		r.remote.putFileAt(r.t, p, content)
	} else {
		r.remote.mkdirAt(r.t, p)
		r.remote.putFileAt(r.t, path.Join(p, modelReplaceChildName), content)
	}
	r.logf("remote replace %s", p)

	return syncfixture.ModelEdit{Side: syncfixture.ModelRemoteSide, Kind: syncfixture.ModelOpReplace, Path: p, Content: content}, true
}

// mirroredMove makes the same rename on both sides, as two devices (or a
// crash after a move ran on one side, D-050) would. Only entries both sides
// still hold at the same path qualify.
func (r *modelRun) mirroredMove() []syncfixture.ModelEdit {
	local := r.localTree()
	remote := r.remote.tree()
	shared := map[string]string{}
	for p, content := range local {
		if remote[p] == content || (content == syncfixture.ModelFolderSentinel && remote[p] == syncfixture.ModelFolderSentinel) {
			shared[p] = content
		}
	}
	from, to, ok := r.pickMove(shared)
	if !ok {
		return nil
	}
	for p := range local {
		if strings.EqualFold(p, to) {
			return nil
		}
	}
	for p := range remote {
		if strings.EqualFold(p, to) {
			return nil
		}
	}
	require.NoError(r.t, os.Rename(
		filepath.Join(r.syncRoot, filepath.FromSlash(from)),
		filepath.Join(r.syncRoot, filepath.FromSlash(to)),
	))
	r.remote.moveAt(r.t, from, to)
	r.logf("both move %s -> %s", from, to)

	return []syncfixture.ModelEdit{
		{Side: syncfixture.ModelLocalSide, Kind: syncfixture.ModelOpMove, Path: from, To: to},
		{Side: syncfixture.ModelRemoteSide, Kind: syncfixture.ModelOpMove, Path: from, To: to},
	}
}

// pickMove chooses an existing entry and a free destination outside its own
// subtree, renaming within a folder or moving to another one.
func (r *modelRun) pickMove(tree map[string]string) (string, string, bool) {
	from := r.pick(append(modelEntriesOf(tree, false), modelEntriesOf(tree, true)...))
	if from == "" {
		return "", "", false
	}
	pool := modelFileNamePool
	if tree[from] == syncfixture.ModelFolderSentinel {
		pool = modelFolderNamePool
	}
	var targets []string
	for _, folder := range modelFoldersOf(tree) {
		if folder == from || strings.HasPrefix(folder, from+"/") {
			continue
		}
		targets = append(targets, folder)
	}
	to := r.newPathIn(targets, pool+" renamed-"+path.Base(from), tree)
	if to == "" || to == from {
		return "", "", false
	}

	return from, to, true
}

// modelPaths lists a tree's paths, folders with a trailing slash.
func modelPaths(tree map[string]string) []string {
	paths := make([]string, 0, len(tree))
	for p, content := range tree {
		if content == syncfixture.ModelFolderSentinel {
			p += "/"
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)

	return paths
}
