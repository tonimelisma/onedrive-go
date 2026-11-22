package sync

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// The watch model: the engine model's batches synced by a running watch
// instead of one-shot passes. Each batch's random edits are made, a watch
// starts over them, and while it runs a second wave arrives (a new local file
// the watcher sees, and a write to a OneDrive file while actions are in
// flight). The batch ends when the watch alone has converged: the two trees
// equal and unchanged over modelWatchStableTicks. The watch then stops and
// the batch takes every check the one-shot model takes, the invariant library
// (assertSyncInvariants) with its cache-consistency check included.
//
// Every run goes through the chaos Graph layer under one seeded fault plan
// (watchFaultPlan): some batches begin in a storm that lasts a few poll
// intervals, and the watch must converge once it passes. Waits the engine
// arms between modelWatchCompressFrom and modelWatchCompressUpTo (retry
// backoff, observer backoff) end in modelWatchCompressedWait of real time on
// the model's clock, the clock moving to their due instant, so a batch's
// failures are retried as a long watch would retry them.
//
// A watch is not replayed exactly: its interleavings follow real time. A
// failing seed still logs its replay line, and rerunning it reruns the same
// edits and the same fault plan.

const (
	modelWatchSeedCountEnv = "ONEDRIVE_GO_WATCH_SEEDS"
	modelWatchSeedStartEnv = "ONEDRIVE_GO_WATCH_SEED_START"
	// modelWatchDefaultSeeds is the plain run's seed count.
	modelWatchDefaultSeeds = 4
	modelWatchBatches      = 4
	// modelWatchPollInterval and modelWatchDebounce pace the watch.
	modelWatchPollInterval = 100 * time.Millisecond
	modelWatchDebounce     = 20 * time.Millisecond
	// modelWatchSettle is how often convergence is checked, and
	// modelWatchStableTicks how many checks in a row must find the same
	// converged trees: several poll intervals and debounce windows, so work
	// still queued has had its chance to show.
	modelWatchSettle      = 100 * time.Millisecond
	modelWatchStableTicks = 8
	// modelWatchCollisionTicks is how many unconverged checks pass before
	// the model's user renames what collides, as syncUntilQuiet's user does.
	modelWatchCollisionTicks = 20
	// modelWatchConvergeWithin bounds one batch's wait.
	modelWatchConvergeWithin = 90 * time.Second
	// modelWatchInFlightWait bounds the wait for the watch's first dispatch
	// before the second wave arrives.
	modelWatchInFlightWait = 2 * time.Second
	// The compressed waits: retry and observer backoff from one second to an
	// hour; the full remote refresh (a day) and the watch's own bounds stay
	// real.
	modelWatchCompressFrom   = time.Second
	modelWatchCompressUpTo   = 2 * time.Hour
	modelWatchCompressedWait = 20 * time.Millisecond
	// modelWatchStormOdds is the chance, one in it, that a batch begins in a
	// storm, and modelWatchMaxStormPolls the most poll intervals it lasts.
	modelWatchStormOdds     = 2
	modelWatchMaxStormPolls = 4
	modelWatchNewFilePrefix = "watch-"
)

// Validates: R-2.3.1, R-2.8.20, R-6.8.9
//
// The primary model test: watch mode converges on every seed, with the
// model's checks and every invariant holding after each batch. The nightly
// seeds profile searches new seeds of it every night;
// ONEDRIVE_GO_WATCH_SEEDS widens the search, from
// ONEDRIVE_GO_WATCH_SEED_START (or name one seed with
// ONEDRIVE_GO_MODEL_SEED).
func TestModel_WatchModeConverges(t *testing.T) {
	syncfixture.SkipInShort(t)
	t.Parallel()

	for _, seed := range watchModelSeeds(t) {
		t.Run("seed="+strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()
			syncfixture.LogSeedReplayOnFailure(t, modelSeedEnv, seed)
			runModelScenarioWith(t, seed, watchModelOptions())
		})
	}
}

// watchModelOptions are the options of every watch model run.
func watchModelOptions() modelOptions {
	batches := modelWatchBatches
	if syncfixture.RaceDetectorEnabled {
		batches = modelRaceBatches
	}

	return modelOptions{watch: true, chaos: true, batches: batches}
}

// watchModelSeedVars are the watch run's variables; its one named seed is
// the engine run's variable.
func watchModelSeedVars() syncfixture.SeedVars {
	return syncfixture.SeedVars{Single: modelSeedEnv, Count: modelWatchSeedCountEnv, Start: modelWatchSeedStartEnv}
}

// watchModelSeeds is the watch run's seeds: the ones the environment names,
// else the first modelWatchDefaultSeeds (one under -race).
func watchModelSeeds(t *testing.T) []uint64 {
	t.Helper()

	count := modelWatchDefaultSeeds
	if syncfixture.RaceDetectorEnabled {
		count = 1
	}

	return syncfixture.SeedsFromEnv(t, watchModelSeedVars(), count)
}

// watchFaultPlan is a watch run's seeded fault plan: for each batch, how long
// it begins in a storm of the chaos layer's faults, zero for a calm batch.
type watchFaultPlan struct {
	storms []time.Duration
}

func newWatchFaultPlan(seed uint64, batches int, chaos bool) watchFaultPlan {
	plan := watchFaultPlan{storms: make([]time.Duration, batches+1)}
	if !chaos {
		return plan
	}
	rng := rand.New(rand.NewPCG(seed^0x510e527fade682d1, seed)) //nolint:gosec // deterministic test RNG
	for batch := 1; batch <= batches; batch++ {
		if rng.IntN(modelWatchStormOdds) == 0 {
			plan.storms[batch] = time.Duration(1+rng.IntN(modelWatchMaxStormPolls)) * modelWatchPollInterval
		}
	}

	return plan
}

// storm is how long batch begins in a storm.
func (p watchFaultPlan) storm(batch int) time.Duration {
	if batch < 0 || batch >= len(p.storms) {
		return 0
	}

	return p.storms[batch]
}

// watchBatch makes one batch's edits and syncs them with a watch, returning
// every edit made, the second wave's and the chaos layer's other client's
// included.
func (r *modelRun) watchBatch(label string, storm time.Duration) []syncfixture.ModelEdit {
	edits := r.applyBatchEdits(label)
	if storm > 0 {
		r.logf("%s: the watch starts in a storm of %s", label, storm)
	}
	if r.chaos != nil {
		r.chaos.startStep(storm > 0)
	}

	localStarted := make(chan struct{}, 1)
	r.eng.SetDebugEventHook(func(event syncdomain.DebugEvent) {
		if event.Type == syncdomain.EngineDebugEventObserverStarted && event.Observer == syncdomain.EngineDebugObserverLocal {
			select {
			case localStarted <- struct{}{}:
			default:
			}
		}
	})
	defer r.eng.SetDebugEventHook(nil)

	r.clock.compressed.Store(true)
	stormEnds := time.Now().Add(storm)
	cancel, done := runWatchInBackground(r.t, r.eng, syncdomain.SyncBidirectional,
		WatchOptions{PollInterval: modelWatchPollInterval, Debounce: modelWatchDebounce})
	syncfixture.ReceiveWithin(r.t, localStarted, label+": the watch's local observer to start")

	r.awaitDispatch()
	edits = append(edits, r.inFlightEdits(label)...)
	edits = append(edits, r.awaitWatchConverged(label, stormEnds)...)

	// The watch's own bounds run on real time while it stops.
	r.clock.compressed.Store(false)
	syncfixture.StopWatch(r.t, cancel, done)

	return append(edits, r.takeConcurrentEdits()...)
}

// awaitDispatch waits for the watch's next dispatch, or modelWatchInFlightWait
// when it has nothing to do.
func (r *modelRun) awaitDispatch() {
	timer := time.NewTimer(modelWatchInFlightWait)
	defer timer.Stop()
	select {
	case <-r.dispatched:
	case <-timer.C:
	}
}

// inFlightEdits is the second wave, made while the watch runs: a new file at
// the top of the sync folder, a name nothing else uses, for the watcher to
// see, and a write to a OneDrive file, when one is still there, while the
// watch's actions are in flight.
func (r *modelRun) inFlightEdits(label string) []syncfixture.ModelEdit {
	var edits []syncfixture.ModelEdit

	content := r.nextContent(syncfixture.ModelLocalSide)
	r.newFiles++
	name := modelWatchNewFilePrefix + strconv.Itoa(r.newFiles) + ".txt"
	file, err := os.OpenFile(filepath.Join(r.syncRoot, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	require.NoError(r.t, err, "%s: creating %s while the watch runs", label, name)
	_, writeErr := file.WriteString(content)
	require.NoError(r.t, errors.Join(writeErr, file.Close()))
	r.logf("%s: local create %s while the watch runs", label, name)
	edits = append(edits, syncfixture.ModelEdit{Side: syncfixture.ModelLocalSide, Kind: syncfixture.ModelOpCreateFile, Path: name, Content: content})

	files := modelEntriesOf(r.remote.tree(), false)
	if len(files) == 0 {
		return edits
	}
	p := r.pick(files)
	remoteContent := r.nextContent(syncfixture.ModelRemoteSide)
	if r.remote.overwriteIfFile(r.t, p, remoteContent) {
		r.logf("%s: remote edit %s while the watch runs", label, p)
		edits = append(edits, syncfixture.ModelEdit{Side: syncfixture.ModelRemoteSide, Kind: syncfixture.ModelOpEditFile, Path: p, Content: remoteContent})
	}

	return edits
}

// overwriteIfFile writes content to the file at remotePath, in one step
// under the fake's lock, when a file is still there; the engine may have
// moved or removed it since the caller looked.
func (r *moveScenarioRemote) overwriteIfFile(t *testing.T, remotePath, content string) bool {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.itemAt(remotePath)
	if !ok || item.IsFolder {
		return false
	}
	r.setContent(t, item.ID, content)

	return true
}

// awaitWatchConverged waits until the watch alone has converged: both trees
// equal, and unchanged, over modelWatchStableTicks checks after the storm has
// ended. A user renames what collides once the watch has been stuck for
// modelWatchCollisionTicks checks; those renames are returned.
func (r *modelRun) awaitWatchConverged(label string, stormEnds time.Time) []syncfixture.ModelEdit {
	deadline := time.NewTimer(modelWatchConvergeWithin)
	defer deadline.Stop()
	tick := time.NewTicker(modelWatchSettle)
	defer tick.Stop()

	var userEdits []syncfixture.ModelEdit
	storming := r.chaos != nil && time.Now().Before(stormEnds)
	stable, stuck := 0, 0
	last := ""
	for {
		select {
		case <-deadline.C:
			require.Failf(r.t, modelNotConvergedMessage, "%s: the watch did not converge within %s; %s\nstate:%s",
				label, modelWatchConvergeWithin, r.failureContext(), syncStateDump(r.t, r.eng))
			return userEdits
		case <-tick.C:
		}
		if storming {
			if time.Now().Before(stormEnds) {
				continue
			}
			r.chaos.startStep(false)
			storming = false
		}

		local, ok := r.liveLocalTree()
		remote := r.remote.tree()
		if !ok || fmt.Sprint(local) != fmt.Sprint(remote) {
			stable, last = 0, ""
			stuck++
			if stuck%modelWatchCollisionTicks == 0 {
				userEdits = append(userEdits, r.resolveNameCollisions()...)
			}
			continue
		}
		stuck = 0
		if snapshot := fmt.Sprint(local); snapshot != last {
			stable, last = 0, snapshot
		}
		stable++
		if stable >= modelWatchStableTicks {
			return userEdits
		}
	}
}

// liveLocalTree is localTree read while the watch may be changing the
// folder: an entry that goes away while it is read makes the reading not
// ok, for the next check to retry.
func (r *modelRun) liveLocalTree() (map[string]string, bool) {
	out := map[string]string{}
	err := filepath.WalkDir(r.syncRoot, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
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

	return out, err == nil
}
