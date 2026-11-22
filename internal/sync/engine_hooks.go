package sync

import (
	"context"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// engineHooks are the engine's test seams, in one place so "what can a test
// make differ from production" has one answer. The zero value is production
// behavior: every nil function, false flag, zero limit and empty name below
// means "do what production does", and production code never sets a field.
// The engine reads them through e.hooks; a test sets them on engineInputs
// before newEngine, or on the engine before it runs.
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
	// decideOverride, when set, replaces syncdomain.Decide in planning. No
	// reachable state is outside the decision table, so a test removes one
	// rule from it to prove the pass fails closed on the row that rule
	// classified (R-6.2.16).
	decideOverride syncstore.Decider
	// shadowPlanned, when set, receives every pass's comparison of its plan
	// with the identity-keyed shadow plan (compareShadowPlan). The models
	// fail on a difference no known reason covers.
	shadowPlanned func(shadowPlanComparison)
	// clearAccountAuthRequirement replaces config.ClearAccountAuthRequirement
	// for the startup proof's clear. Tests return a held catalog lock
	// through it without waiting out the lock's timeout.
	clearAccountAuthRequirement func(dataDir string, account driveid.CanonicalID) error
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
		syncdomain.SocketIOEndpointFetcher,
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
}

// runClock is the clock a run reads: the engine's real clock unless a test
// replaced it.
func (e *Engine) runClock() syncdomain.SyncClock {
	if e.hooks.clock != nil {
		return e.hooks.clock
	}

	return e.wallClock
}
