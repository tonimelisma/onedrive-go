package sync

import (
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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
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
// Failures print the seed; rerun one with ONEDRIVE_GO_MODEL_SEED=<seed> and
// widen the search with ONEDRIVE_GO_MODEL_SEEDS=<count>, starting at
// ONEDRIVE_GO_MODEL_SEED_START=<seed> (default 1). The default run is
// modelDefaultSeeds, cheap enough for every CI run; a nightly or local search
// sets the count. Each seed that found a bug is kept as a named scenario test
// (engine_move_scenario_test.go), because adding operations renumbers what a
// seed does.
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
	modelBatches             = 6
	modelMaxOpsPerSide       = 3
	modelMaxSyncPasses       = 8
	modelDefaultSeeds        = 12
	modelCrashSeedCount      = 4
	modelRaceSeeds           = 2
	modelInitialFolders      = 3
	modelInitialFiles        = 5
	modelFolderSentinel      = "/"
	modelSeedEnv             = "ONEDRIVE_GO_MODEL_SEED"
	modelSeedCountEnv        = "ONEDRIVE_GO_MODEL_SEEDS"
	modelSeedStartEnv        = "ONEDRIVE_GO_MODEL_SEED_START"
	modelTraceEnv            = "ONEDRIVE_GO_MODEL_TRACE"
	modelConflictMarker      = ".conflict-"
	modelLocalSide           = "local"
	modelRemoteSide          = "remote"
	modelFileNamePool        = "a.txt b.txt c.md 50%.txt a+b.txt"
	modelFolderNamePool      = "docs photos x q%20r"
	modelOpCreateFile        = "create-file"
	modelOpEditFile          = "edit-file"
	modelOpDelete            = "delete"
	modelOpMove              = "move"
	modelOpMakeFolder        = "mkdir"
	modelOpReplace           = "replace"
	modelReplaceChildName    = "inside.txt"
	modelMirroredMoveOdds    = 3
	modelNotConvergedMessage = "sync did not converge"
)

// Validates: R-2.3.1, R-2.17.1, R-2.17.2, R-2.17.3, R-2.17.6, R-2.17.7, R-2.17.8
func TestRunOnce_RandomizedBidirectionalEditsConverge(t *testing.T) {
	t.Parallel()

	for _, seed := range modelSeeds(t) {
		t.Run("seed="+strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()
			runModelScenario(t, seed)
		})
	}
}

// Validates: R-2.3.1
//
// The same seed twice gives the same actions in the same order and the same
// trees (D-090): a failing seed is a reproducible bug, not a flake.
func TestModel_SameSeedReplaysIdentically(t *testing.T) {
	t.Parallel()

	for _, seed := range []uint64{3, 28} {
		t.Run("seed="+strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()

			var transcripts [2][]string
			for i := range transcripts {
				t.Run("run="+strconv.Itoa(i+1), func(t *testing.T) {
					transcripts[i] = runModelScenario(t, seed).transcript
				})
			}
			require.NotEmpty(t, transcripts[0])
			assert.Equal(t, transcripts[0], transcripts[1], "two runs of seed %d diverged", seed)
		})
	}
}

// Validates: R-6.8.9
//
// Every batch of a few seeds also crashes at a crash point the seed picks
// (D-091), sometimes losing the WAL too, and sync must converge from what the
// crash left with every model check and invariant holding.
func TestModel_CrashAtSeededPointsRecovers(t *testing.T) {
	t.Parallel()

	for _, seed := range modelCrashSeeds(t) {
		t.Run("seed="+strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()
			runModelScenarioWith(t, seed, true)
		})
	}
}

// modelCrashSeeds is the crash run's seeds: the ones the environment names,
// else the first modelCrashSeedCount (one under -race).
func modelCrashSeeds(t *testing.T) []uint64 {
	t.Helper()

	seeds := modelSeeds(t)
	if os.Getenv(modelSeedEnv) != "" || os.Getenv(modelSeedCountEnv) != "" {
		return seeds
	}
	limit := modelCrashSeedCount
	if raceDetectorEnabled {
		// The race run is for data races, which one seed exercises; see
		// modelRaceSeeds.
		limit = 1
	}

	return seeds[:min(limit, len(seeds))]
}

func modelSeeds(t *testing.T) []uint64 {
	t.Helper()

	if raw := os.Getenv(modelSeedEnv); raw != "" {
		seed, err := strconv.ParseUint(raw, 10, 64)
		require.NoError(t, err, modelSeedEnv)
		return []uint64{seed}
	}
	count := modelDefaultSeeds
	if raceDetectorEnabled {
		count = modelRaceSeeds
	}
	if raw := os.Getenv(modelSeedCountEnv); raw != "" {
		parsed, err := strconv.Atoi(raw)
		require.NoError(t, err, modelSeedCountEnv)
		count = parsed
	}
	start := uint64(1)
	if raw := os.Getenv(modelSeedStartEnv); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 64)
		require.NoError(t, err, modelSeedStartEnv)
		start = parsed
	}
	seeds := make([]uint64, count)
	for i := range seeds {
		seeds[i] = start + uint64(i)
	}

	return seeds
}

// modelEdit is one operation a side applied, recorded for the invariants.
type modelEdit struct {
	side    string
	kind    string
	path    string
	to      string // destination of a move
	content string // bytes written by create/edit
}

// touches reports whether the edit affected p, its ancestors or its subtree.
func (e modelEdit) touches(p string) bool {
	for _, affected := range []string{e.path, e.to} {
		if affected == "" {
			continue
		}
		if p == affected || strings.HasPrefix(p, affected+"/") || strings.HasPrefix(affected, p+"/") {
			return true
		}
	}

	return false
}

type modelRun struct {
	t        *testing.T
	seed     uint64
	rng      *rand.Rand
	remote   *moveScenarioRemote
	eng      *testEngine
	syncRoot string
	harness  *syncHarness
	writes   int
	log      []string

	// caseSensitive gates the operations only a case-sensitive local
	// filesystem can express.
	caseSensitive bool
	// expectHoldOf is the side a wipe emptied: the next pass must hold the
	// deletes it would propagate.
	expectHoldOf string
	// afterFirstPass is a user's response to what the first pass did (a
	// collision to rename); its edits join the batch's.
	afterFirstPass func() []modelEdit
	// clock is the engine's clock, a passage of time ahead of the wall clock
	// that grows by modelPassInterval before every pass after a batch's
	// first.
	clock *modelClock

	// transcript is every dispatched action and every converged tree, in
	// order, with conflict-copy timestamps masked: two runs of one seed must
	// produce the same transcript.
	transcriptMu stdsync.Mutex
	transcript   []string
}

func runModelScenario(t *testing.T, seed uint64) *modelRun {
	t.Helper()

	return runModelScenarioWith(t, seed, false)
}

// runModelScenarioWith runs one seed. With crashes set, every batch also
// crashes at a crash point chosen by the seed (D-091): sync continues on a
// fresh engine opened on what the crash left, and the batch's checks apply
// to it.
func runModelScenarioWith(t *testing.T, seed uint64, crashes bool) *modelRun {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.shuffleDelta(seed)
	h := newSyncHarness(t, remote)
	run := &modelRun{
		t: t, seed: seed, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), //nolint:gosec // deterministic test RNG
		remote: remote, syncRoot: h.syncRoot, harness: h,
		caseSensitive: localFSIsCaseSensitive(t.TempDir()),
		clock:         &modelClock{},
	}
	run.adoptEngine(h.eng)
	crashRng := rand.New(rand.NewPCG(seed^0xc2b2ae3d27d4eb4f, seed)) //nolint:gosec // deterministic test RNG

	run.seedInitialTree()
	run.syncUntilQuiet("initial")
	run.requireConverged("initial")

	for batch := 1; batch <= modelBatches; batch++ {
		label := fmt.Sprintf("batch %d", batch)
		before := run.localTree()
		h.beginRound(t)
		edits := run.applyBatchEdits(label)
		var capture *crashCapture
		if crashes {
			capture = run.armCrash(crashRng)
		}
		edits = append(edits, run.syncUntilQuiet(label)...)
		if capture != nil && capture.captured {
			h = run.recoverFrom(capture, label)
			edits = append(edits, run.syncUntilQuiet(label+" after the crash")...)
		}
		converged := run.requireConverged(label)
		run.requireWrittenContentSurvives(label, edits, converged)
		run.requireUntouchedFilesUnchanged(label, before, edits, converged)
		if !assertSyncInvariants(t, h) {
			require.FailNow(t, "sync invariants violated", "%s: %s", label, run.failureContext())
		}
		run.record("%s converged: %v", label, modelPaths(converged))
	}

	return run
}

// adoptEngine makes eng the model's engine: the model's clock, one worker,
// and the seed's dispatch order, recorded.
func (r *modelRun) adoptEngine(eng *testEngine) {
	r.eng = eng
	eng.clock = r.clock
	order := eng.dispatchOrder
	if order == nil {
		order = dispatchOrderShuffle(r.seed)
	}
	eng.serializeWorkers = true
	eng.dispatchOrder = func(frontier []*trackedAction) []*trackedAction {
		frontier = order(frontier)
		r.recordDispatch(frontier)
		return frontier
	}
}

// armCrash installs a capture at a crash point the seed picks, half the time
// with power loss (everything since a checkpoint taken now is lost).
func (r *modelRun) armCrash(rng *rand.Rand) *crashCapture {
	points := []string{
		crashPointRemoteMoveDone, crashPointLocalMoveDone, crashPointUploadRemoteDone,
		crashPointDownloadRenamed, crashPointCommitObservation, crashPointCommitOutcome,
	}
	capture := &crashCapture{
		point:     points[rng.IntN(len(points))],
		powerLoss: rng.IntN(2) == 0,
		srcDir:    filepath.Dir(r.syncRoot),
		dstDir:    r.t.TempDir(),
		remote:    r.remote,
	}
	if capture.powerLoss {
		_, err := r.eng.baseline.rawDB().ExecContext(r.t.Context(), `PRAGMA wal_checkpoint(TRUNCATE)`)
		require.NoError(r.t, err)
	}
	r.eng.execCfg.crashAt = capture.hook
	r.eng.baseline.crashAt = capture.hook

	return capture
}

// recoverFrom continues the run on a fresh engine opened on what the crash
// left behind.
func (r *modelRun) recoverFrom(capture *crashCapture, label string) *syncHarness {
	kind := "crash"
	if capture.powerLoss {
		kind = "power loss"
	}
	r.logf("%s: %s at %s; a fresh engine opens what it left", label, kind, capture.point)
	held := r.harness.held
	recovered := capture.reopen(r.t)
	recovered.held = held
	r.harness = recovered
	r.remote = recovered.remote
	r.syncRoot = recovered.syncRoot
	r.adoptEngine(recovered.eng)

	return recovered
}

// modelConflictStamp is the timestamp in a conflict copy's name, which comes
// from the wall clock and so differs between two runs of one seed.
var modelConflictStamp = regexp.MustCompile(`\.conflict-\d{8}-\d{6}`)

func (r *modelRun) record(format string, args ...any) {
	line := modelConflictStamp.ReplaceAllString(fmt.Sprintf(format, args...), ".conflict-STAMP")
	r.transcriptMu.Lock()
	defer r.transcriptMu.Unlock()
	r.transcript = append(r.transcript, line)
}

func (r *modelRun) recordDispatch(frontier []*trackedAction) {
	for _, ta := range frontier {
		r.record("dispatch %s %s <- %s", ta.Action.Type, ta.Action.Path, ta.Action.OldPath)
	}
}

func (r *modelRun) logf(format string, args ...any) {
	r.log = append(r.log, fmt.Sprintf(format, args...))
}

func (r *modelRun) failureContext() string {
	return fmt.Sprintf("seed %d (rerun with %s=%d); operations:\n  %s",
		r.seed, modelSeedEnv, r.seed, strings.Join(r.log, "\n  "))
}

func (r *modelRun) nextContent(side string) string {
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
			r.remote.putFileAt(r.t, file, r.nextContent(modelRemoteSide))
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
func (r *modelRun) syncUntilQuiet(label string) []modelEdit {
	confirm := false
	var userEdits []modelEdit
	for pass := 1; pass <= modelMaxSyncPasses; pass++ {
		if pass > 1 {
			r.clock.advance(modelPassInterval)
		}
		report, err := r.eng.RunOnce(r.t.Context(), SyncBidirectional, RunOptions{ConfirmDeletes: confirm})
		require.NoError(r.t, err, "%s pass %d: %s", label, pass, r.failureContext())
		held := report.HeldLocalDeletes + report.HeldRemoteDeletes
		confirm = held > 0
		changed := report.Uploads + report.Downloads + report.LocalDeletes + report.RemoteDeletes +
			report.FolderCreates + report.Moves + held
		if pass == 1 {
			r.requireWipeHeld(label, report)
			if r.afterFirstPass != nil {
				userEdits = append(userEdits, r.afterFirstPass()...)
				r.afterFirstPass = nil
				changed++
			}
		}
		if os.Getenv(modelTraceEnv) != "" {
			r.t.Logf("%s pass %d: up=%d down=%d ldel=%d rdel=%d mkdir=%d mv=%d ok=%d failed=%d errors=%v\n  local:  %v\n  remote: %v",
				label, pass, report.Uploads, report.Downloads, report.LocalDeletes, report.RemoteDeletes,
				report.FolderCreates, report.Moves, report.Succeeded, report.Failed, report.Errors,
				modelPaths(r.localTree()), modelPaths(r.remote.tree()))
		}
		// The user renames what the engine reports as a name collision,
		// once the pass is quiet or the collision is what failed.
		quiet := changed == 0 && report.Failed == 0
		if quiet || (report.Failed > 0 && pass > 1) {
			resolved := r.resolveNameCollisions()
			if quiet && len(resolved) == 0 {
				return userEdits
			}
			userEdits = append(userEdits, resolved...)
		}
	}
	require.Failf(r.t, modelNotConvergedMessage, "%s: still changing after %d passes; %s\nstate:%s",
		label, modelMaxSyncPasses, r.failureContext(), syncStateDump(r.t, r.eng))

	return userEdits
}

// modelPassInterval is how much later the model's user runs each further
// pass of a batch: past the longest retry backoff (one hour), so work an
// earlier pass failed is due again, as it would be by the time a user, or
// watch mode, syncs next. Without it a retry held by its backoff read as
// non-convergence after eight instant passes.
const modelPassInterval = time.Hour + time.Minute

// modelClock is the wall clock plus an offset the model advances.
type modelClock struct {
	realClock
	mu     stdsync.Mutex
	offset time.Duration
}

func (c *modelClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return time.Now().Add(c.offset)
}

func (c *modelClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// requireWipeHeld checks that the first pass after a side was wiped held the
// deletes instead of propagating them (D-002).
func (r *modelRun) requireWipeHeld(label string, report *Report) {
	switch r.expectHoldOf {
	case modelLocalSide:
		require.Positive(r.t, report.HeldRemoteDeletes, "%s: a wiped local side must hold the OneDrive deletes; %s", label, r.failureContext())
	case modelRemoteSide:
		require.Positive(r.t, report.HeldLocalDeletes, "%s: a wiped OneDrive must hold the local deletes; %s", label, r.failureContext())
	}
	r.expectHoldOf = ""
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
func (r *modelRun) requireWrittenContentSurvives(label string, edits []modelEdit, converged map[string]string) {
	present := make(map[string]bool, len(converged))
	for _, content := range converged {
		present[content] = true
	}
	for i, edit := range edits {
		if edit.content == "" || modelOverwrittenLater(edits, i) {
			continue
		}
		assert.True(r.t, present[edit.content],
			"%s: %s %s of %s was lost; %s", label, edit.side, edit.kind, edit.path, r.failureContext())
	}
}

// modelOverwrittenLater reports whether the same side later replaced or
// removed what edits[i] wrote, in which case losing it is correct.
func modelOverwrittenLater(edits []modelEdit, i int) bool {
	for _, later := range edits[i+1:] {
		if later.side != edits[i].side {
			continue
		}
		if later.touches(edits[i].path) && later.kind != modelOpMakeFolder {
			return true
		}
	}

	return false
}

// requireUntouchedFilesUnchanged checks invariant 3.
func (r *modelRun) requireUntouchedFilesUnchanged(
	label string,
	before map[string]string,
	edits []modelEdit,
	converged map[string]string,
) {
	for p, content := range before {
		if content == modelFolderSentinel {
			continue
		}
		touched := false
		for _, edit := range edits {
			if edit.touches(p) {
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
	out := map[string]string{}
	require.NoError(r.t, filepath.WalkDir(r.syncRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(r.syncRoot, p)
		if relErr != nil {
			return fmt.Errorf("relative path of %s: %w", p, relErr)
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			out[rel] = modelFolderSentinel
			return nil
		}
		data, readErr := os.ReadFile(p) //nolint:gosec // test reads its own temp tree
		if readErr != nil {
			return fmt.Errorf("read %s: %w", p, readErr)
		}
		out[rel] = string(data)
		return nil
	}))

	return out
}

func (r *modelRun) remoteTreeView() map[string]string { return r.remote.tree() }

func (r *modelRun) remoteFolders() []string { return modelFoldersOf(r.remote.tree()) }

func (r *modelRun) localFolders() []string { return modelFoldersOf(r.localTree()) }

func modelFoldersOf(tree map[string]string) []string {
	folders := []string{""}
	for p, content := range tree {
		if content == modelFolderSentinel {
			folders = append(folders, p)
		}
	}
	sort.Strings(folders)

	return folders
}

func modelEntriesOf(tree map[string]string, wantFolders bool) []string {
	var entries []string
	for p, content := range tree {
		if (content == modelFolderSentinel) == wantFolders {
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
func (r *modelRun) localBasicEdit(kind string, tree map[string]string) (modelEdit, bool) {
	switch kind {
	case modelOpReplace:
		return r.localReplace(tree)
	case modelOpCreateFile:
		p := r.newPathIn(r.localFolders(), modelFileNamePool, tree)
		if p == "" {
			return modelEdit{}, false
		}
		content := r.nextContent(modelLocalSide)
		writeLocalFile(r.t, r.syncRoot, filepath.FromSlash(p), content)
		r.logf("local create %s", p)
		return modelEdit{side: modelLocalSide, kind: modelOpCreateFile, path: p, content: content}, true
	case modelOpDelete:
		p := r.pick(append(modelEntriesOf(tree, false), modelEntriesOf(tree, true)...))
		if p == "" {
			return modelEdit{}, false
		}
		require.NoError(r.t, os.RemoveAll(filepath.Join(r.syncRoot, filepath.FromSlash(p))))
		r.logf("local delete %s", p)
		return modelEdit{side: modelLocalSide, kind: modelOpDelete, path: p}, true
	case modelOpMove:
		return r.localMove(tree)
	default:
		p := r.newPathIn(r.localFolders(), modelFolderNamePool, tree)
		if p == "" {
			return modelEdit{}, false
		}
		require.NoError(r.t, os.MkdirAll(filepath.Join(r.syncRoot, filepath.FromSlash(p)), 0o750))
		r.logf("local mkdir %s", p)
		return modelEdit{side: modelLocalSide, kind: modelOpMakeFolder, path: p}, true
	}
}

func (r *modelRun) localMove(tree map[string]string) (modelEdit, bool) {
	from, to, ok := r.pickMove(tree)
	if !ok {
		return modelEdit{}, false
	}
	require.NoError(r.t, os.Rename(
		filepath.Join(r.syncRoot, filepath.FromSlash(from)),
		filepath.Join(r.syncRoot, filepath.FromSlash(to)),
	))
	r.logf("local move %s -> %s", from, to)

	return modelEdit{side: modelLocalSide, kind: modelOpMove, path: from, to: to}, true
}

// remoteBasicEdit is one of the original remote operations.
func (r *modelRun) remoteBasicEdit(kind string, tree map[string]string) (modelEdit, bool) {
	switch kind {
	case modelOpReplace:
		return r.remoteReplace(tree)
	case modelOpCreateFile:
		p := r.newPathIn(modelFoldersOf(tree), modelFileNamePool, tree)
		if p == "" {
			return modelEdit{}, false
		}
		content := r.nextContent(modelRemoteSide)
		r.remote.putFileAt(r.t, p, content)
		r.logf("remote create %s", p)
		return modelEdit{side: modelRemoteSide, kind: modelOpCreateFile, path: p, content: content}, true
	case modelOpEditFile:
		return r.remoteEditFile(tree)
	case modelOpDelete:
		p := r.pick(append(modelEntriesOf(tree, false), modelEntriesOf(tree, true)...))
		if p == "" {
			return modelEdit{}, false
		}
		r.remote.deleteAt(r.t, p)
		r.logf("remote delete %s", p)
		return modelEdit{side: modelRemoteSide, kind: modelOpDelete, path: p}, true
	case modelOpMove:
		from, to, ok := r.pickMove(tree)
		if !ok {
			return modelEdit{}, false
		}
		r.remote.moveAt(r.t, from, to)
		r.logf("remote move %s -> %s", from, to)
		return modelEdit{side: modelRemoteSide, kind: modelOpMove, path: from, to: to}, true
	default:
		p := r.newPathIn(modelFoldersOf(tree), modelFolderNamePool, tree)
		if p == "" {
			return modelEdit{}, false
		}
		r.remote.mkdirAt(r.t, p)
		r.logf("remote mkdir %s", p)
		return modelEdit{side: modelRemoteSide, kind: modelOpMakeFolder, path: p}, true
	}
}

// localReplace changes an entry's type in place: a file becomes a folder with
// one new file in it, a folder (with its subtree) becomes a file.
func (r *modelRun) localReplace(tree map[string]string) (modelEdit, bool) {
	p := r.pick(append(modelEntriesOf(tree, false), modelEntriesOf(tree, true)...))
	if p == "" {
		return modelEdit{}, false
	}
	abs := filepath.Join(r.syncRoot, filepath.FromSlash(p))
	require.NoError(r.t, os.RemoveAll(abs))
	content := r.nextContent(modelLocalSide)
	if tree[p] == modelFolderSentinel {
		writeLocalFile(r.t, r.syncRoot, filepath.FromSlash(p), content)
	} else {
		writeLocalFile(r.t, r.syncRoot, filepath.FromSlash(path.Join(p, modelReplaceChildName)), content)
	}
	r.logf("local replace %s", p)

	return modelEdit{side: modelLocalSide, kind: modelOpReplace, path: p, content: content}, true
}

func (r *modelRun) remoteReplace(tree map[string]string) (modelEdit, bool) {
	p := r.pick(append(modelEntriesOf(tree, false), modelEntriesOf(tree, true)...))
	if p == "" {
		return modelEdit{}, false
	}
	r.remote.deleteAt(r.t, p)
	content := r.nextContent(modelRemoteSide)
	if tree[p] == modelFolderSentinel {
		r.remote.putFileAt(r.t, p, content)
	} else {
		r.remote.mkdirAt(r.t, p)
		r.remote.putFileAt(r.t, path.Join(p, modelReplaceChildName), content)
	}
	r.logf("remote replace %s", p)

	return modelEdit{side: modelRemoteSide, kind: modelOpReplace, path: p, content: content}, true
}

// mirroredMove makes the same rename on both sides, as two devices (or a
// crash after a move ran on one side, D-050) would. Only entries both sides
// still hold at the same path qualify.
func (r *modelRun) mirroredMove() []modelEdit {
	local := r.localTree()
	remote := r.remote.tree()
	shared := map[string]string{}
	for p, content := range local {
		if remote[p] == content || (content == modelFolderSentinel && remote[p] == modelFolderSentinel) {
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

	return []modelEdit{
		{side: modelLocalSide, kind: modelOpMove, path: from, to: to},
		{side: modelRemoteSide, kind: modelOpMove, path: from, to: to},
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
	if tree[from] == modelFolderSentinel {
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
		if content == modelFolderSentinel {
			p += "/"
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)

	return paths
}
