package sync

import (
	"context"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncplan"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// engineHooks are the engine's test seams, in one place so "what can a test
// make differ from production" has one answer. The zero value is production
// behavior: every nil function, false flag, zero limit and empty name below
// means "do what production does", and production code never sets a field.
// A run reads them from the copy its flow took when it started
// (flowInputs, flow.hooks), never from the live e.hooks, so a hook a test sets
// on the engine reaches the next run and not one already running. Only reads
// that happen before a run has a flow (the owner-check factory in beginRun,
// the engine's own clock between runs) read e.hooks. A test sets them on
// engineInputs before newEngine, or on the engine before it runs.
type engineHooks struct {
	// assertInvariants runs the runtime invariant checks after every
	// completion and admission, so a lifecycle or scope regression fails the
	// test where it happens.
	assertInvariants bool
	// ownerCheckFactory is called on the goroutine that starts a run
	// (beginRun) and returns the owner check of that run, which the run's
	// scope ledger, watch timers and every dependency graph it builds carry.
	// A test proves the run touches them from its own goroutine alone.
	ownerCheckFactory func() func()
	// dispatchOrder reorders every ready frontier before it is dispatched
	// (D-090). Tests run scenarios forward, reversed or in a seeded shuffle
	// so a bug that depends on dispatch order cannot pass by timing; nil
	// keeps the dependency graph's order.
	dispatchOrder func([]*syncdomain.TrackedAction) []*syncdomain.TrackedAction
	// recordFrontier is told, in order, every publication-only action the
	// engine applies and then every action a frontier dispatches, after
	// dispatchOrder: one-action and publication-only frontiers included, so
	// a model's kept log shows every pass. It receives a copy of each
	// action, so it records but cannot reorder or change what runs; nil
	// records nothing.
	recordFrontier func(step frontierStep, action *syncdomain.Action)
	// serializeWorkers runs one worker instead of the pool, so with a
	// dispatch order execution is deterministic.
	serializeWorkers bool
	// oneShotPassLimit, when positive, lowers the passes one RunOnce makes
	// below oneShotMaxPasses: a test that inspects what a single pass
	// committed sets it to 1.
	oneShotPassLimit int
	// wait is how a one-shot sync waits out a scope's pause before its next
	// pass (oneShotScopeWait); nil waits on a real timer, a test advances its
	// manual clock instead.
	wait func(ctx context.Context, d time.Duration) error
	// decideOverride, when set, replaces syncplan.Decide in planning. No
	// reachable state is outside the decision table, so a test removes one
	// rule from it to prove the pass fails closed on the row that rule
	// classified (R-6.2.16).
	decideOverride syncplan.Decider
	// clearAccountAuthRequirement replaces config.ClearAccountAuthRequirement
	// for the startup proof's clear. Tests return a held catalog lock
	// through it without waiting out the lock's timeout.
	clearAccountAuthRequirement func(ctx context.Context, dataDir string, account driveid.CanonicalID) error
	// clock replaces the engine's run clock (runClock); nil runs on the
	// engine's real clock. Observers the engine builds stay on the real
	// clock either way (buildLocalObserver).
	clock syncdomain.SyncClock
	// exec, store and observe are the test seams of the executor, the state
	// store and the local observers the engine builds, passed through to
	// each unchanged.
	exec    syncexec.Hooks
	store   syncstore.Hooks
	observe syncobserve.Hooks
	// socketIOWakeSourceFactory replaces the watch-mode websocket wake
	// source (newSocketIOWakeSource).
	socketIOWakeSourceFactory func(
		SocketIOEndpointFetcher,
		driveid.ID,
		syncobserve.SocketIOWakeSourceOptions,
	) socketIOWakeSourceRunner
	// lockFlock replaces syscall.Flock for the engine's two locks, on its
	// sync folder and on its state store. Tests inject
	// unsupported-filesystem errnos through it and watch the unlocks.
	lockFlock func(fd int, how int) error
	// interposeSyncTree wraps the sync tree's filesystem handle. Tests put an
	// emulated filesystem under the engine through it: a case-insensitive
	// volume, injected errno (D-094).
	interposeSyncTree func(synctree.Handle) synctree.Handle
	// adaptSyncTree replaces the sync tree with one of its test variants
	// (synctree.Root.WithoutNoReplaceFlagAndUnremovableName), after
	// interposeSyncTree: a system that refuses the no-replace flag and a name
	// that cannot be unlinked.
	adaptSyncTree func(*synctree.Root) *synctree.Root
}

// frontierStep names what the engine did with an action recordFrontier is
// told about.
type frontierStep string

const (
	// frontierPublished is a publication-only action the engine applied on
	// its own side of the runtime boundary, without a worker.
	frontierPublished frontierStep = "publish"
	// frontierDispatched is an action handed to the workers.
	frontierDispatched frontierStep = "dispatch"
)

// recordFrontierStep tells the recordFrontier hook, when a test set one,
// what the engine did with ta.
func (h *engineHooks) recordFrontierStep(step frontierStep, ta *syncdomain.TrackedAction) {
	if h.recordFrontier == nil || ta == nil {
		return
	}

	// A copy, so the record cannot change or reorder what runs.
	action := ta.Action
	h.recordFrontier(step, &action)
}

// runClock is the clock a run reads: the engine's real clock unless a test
// replaced it. It reads the engine's live hooks, so it serves the engine
// between runs; a run reads its flow's clock (runtimeDeps.clock).
func (e *Engine) runClock() syncdomain.SyncClock {
	return e.hooks.runClock(e.wallClock)
}

// runClock is the clock these hooks give a run: wall unless a test replaced
// it.
func (h *engineHooks) runClock(wall syncdomain.RealClock) syncdomain.SyncClock {
	if h.clock != nil {
		return h.clock
	}

	return wall
}

// workerPoolSize is how many workers a run's pools run with these hooks: the
// configured transfer workers with the pool's floor, or one when serialized
// (D-090). It is the ceiling a fresh plan's slow start doubles toward
// (R-6.8.25).
func (h *engineHooks) workerPoolSize(transferWorkers int) int {
	return syncexec.PoolWorkerCount(transferWorkers, h.serializeWorkers)
}

// oneShotPasses is the most passes one RunOnce makes with these hooks.
func (h *engineHooks) oneShotPasses() int {
	if h.oneShotPassLimit > 0 {
		return min(h.oneShotPassLimit, oneShotMaxPasses)
	}

	return oneShotMaxPasses
}
