package sync

import (
	"sync/atomic"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncplan"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// engineFlow holds mutable per-run execution state shared by one-shot and
// watch coordinators. It holds no pointer to the engine: everything it reads
// of the engine (configuration, hooks, capabilities, collaborators and the
// engine-owned state that guards itself) it was handed by name when the run
// started (flowInputs).
type engineFlow struct {
	// observation is the engine's observing collaborators, the run's
	// configuration and its capabilities: everything code off the loop
	// goroutine may reach (engine_observation.go).
	observation

	// hooks are the engine's test seams as they were when the run started.
	hooks engineHooks

	// The collaborators only the loop goroutine uses.
	planner *syncplan.Planner
	exec    *syncexec.ExecutorConfig
	perms   *permissionHandler
	upkeep  *housekeeper
	// ownerCheck is the run's owner check (beginRun), nil in production.
	ownerCheck func()

	// signals is how this flow asks for future work. Watch supplies the live
	// runtime; one-shot supplies no-ops, because a one-shot pass has no future
	// in which a timer could fire. See runtimeSignals.
	signals runtimeSignals

	// inspect is the debug-only view of watch state used by the invariant
	// pass, which production leaves disabled.
	inspect watchInspector

	// admitted is the runtime holding admitted, not yet dispatched work. A
	// block scope that activates takes back what it blocks through it
	// (R-6.8.24). Watch and one-shot each supply their own outbox; a bare
	// flow has none.
	admitted admittedWorkOwner

	// The four owners below split this run's mutable state by authority; see
	// engine_runtime_owners.go. One goroutine still owns all four.
	sched   actionScheduler
	retries retryLedger
	scopes  scopeLedger
	// retryWork is the run's one path to the retry_work table and its cache.
	retryWork retryWork
	results   runResults

	runID string

	// supersedeStreaks counts consecutive superseded completions per action
	// key (type, old path, path) and resets when that action succeeds. A
	// superseded action leaves no retry row and no user-facing output, so an
	// action that is superseded on every replan would otherwise stall
	// silently (a folder move once did, on a churning folder eTag).
	supersedeStreaks map[string]int

	// stop is the one bound a stopping run records the results of its
	// completions under (beginStopDrain); nil while the run goes on.
	stop *stopDrain

	// stopBound is how long that bound lasts: OutcomeCommitTimeout, which
	// tests shorten.
	stopBound time.Duration

	// canceledAt is when the run's context ended, set once by the callback
	// noteRunCancel registers; nil while the run goes on. The stop's bound
	// starts there.
	canceledAt atomic.Pointer[time.Time]
}

// runConfig is the engine configuration a run reads: plain values fixed when
// the engine was built, copied once when the run starts.
type runConfig struct {
	driveID          driveid.ID
	remoteRootItemID string
	dataDir          string
	syncRoot         string
	syncTree         *synctree.Root
	transferWorkers  int
	// poolSize is how many workers the run's pools run (workerPoolSize).
	poolSize            int
	checkWorkers        int
	contentFilter       syncdomain.ContentFilterConfig
	localRules          syncdomain.LocalObservationRules
	shortcutNamespaceID string
	enableWebsocket     bool
	// accountCanonicalID names the account whose sign-in a rejected token
	// marks as needing attention.
	accountCanonicalID driveid.CanonicalID
}

// flowInputs is everything newEngineFlow builds a run's flow from, each by
// name.
type flowInputs struct {
	cfg   runConfig
	deps  runtimeDeps
	hooks engineHooks
	runID string

	roots     *protectedRootSet
	remote    *remoteSource
	local     *localSource
	shortcuts *shortcutRoots

	planner    *syncplan.Planner
	exec       *syncexec.ExecutorConfig
	perms      *permissionHandler
	upkeep     *housekeeper
	ownerCheck func()
}

// flowInputs is the one place a run reads the engine's configuration, hooks
// and capabilities. Each call takes the next run ID.
func (e *Engine) flowInputs() *flowInputs {
	return &flowInputs{
		cfg: runConfig{
			driveID:             e.driveID,
			remoteRootItemID:    e.remoteRootItemID,
			dataDir:             e.dataDir,
			syncRoot:            e.syncRoot,
			syncTree:            e.syncTree,
			transferWorkers:     e.transferWorkers,
			poolSize:            e.workerPoolSize(),
			checkWorkers:        e.checkWorkers,
			contentFilter:       e.contentFilter,
			localRules:          e.localRules,
			shortcutNamespaceID: e.shortcutNamespaceID,
			enableWebsocket:     e.enableWebsocket,
			accountCanonicalID:  e.accountCanonicalID,
		},
		deps:  newRuntimeDeps(e),
		hooks: e.hooks,
		runID: e.nextRuntimeRunID(),

		roots:     e.protectedRoots,
		remote:    e.remoteSource(),
		local:     e.localSource(),
		shortcuts: e.shortcutRoots(),

		planner:    e.planner,
		exec:       e.execCfg,
		perms:      e.permHandler,
		upkeep:     e.upkeep,
		ownerCheck: e.runOwnerCheck,
	}
}

func newEngineFlow(in *flowInputs) *engineFlow {
	flow := &engineFlow{
		observation: observation{
			cfg:       in.cfg,
			deps:      in.deps,
			roots:     in.roots,
			remote:    in.remote,
			local:     in.local,
			shortcuts: in.shortcuts,
		},
		hooks:      in.hooks,
		planner:    in.planner,
		exec:       in.exec,
		perms:      in.perms,
		upkeep:     in.upkeep,
		ownerCheck: in.ownerCheck,
		signals:    noRuntimeSignals{},
		inspect:    noWatchInspector{},
		admitted:   noAdmittedWork{},
		runID:      in.runID,
		sched:      newActionScheduler(),
		retries:    newRetryLedger(),
		scopes:     scopeLedger{ownerCheck: in.ownerCheck},
		retryWork:  retryWork{rows: make(map[syncdomain.RetryWorkKey]syncdomain.RetryWorkRow)},

		supersedeStreaks: make(map[string]int),
		stopBound:        syncdomain.OutcomeCommitTimeout,
	}
	return flow
}

type oneShotRunner struct {
	*engineFlow

	// outbox is the admitted work the results loop has not handed to a
	// worker yet. Owned by the results loop goroutine; a scope activation
	// applied by that goroutine takes blocked work back out of it.
	outbox []*syncdomain.TrackedAction
}

func newOneShotRunner(engine *Engine) *oneShotRunner {
	runner := &oneShotRunner{
		engineFlow: newEngineFlow(engine.flowInputs()),
	}
	runner.admitted = runner

	return runner
}

// takeAdmittedWorkBlockedBy satisfies admittedWorkOwner for the one-shot
// results loop.
func (r *oneShotRunner) takeAdmittedWorkBlockedBy(key syncdomain.ScopeKey) []*syncdomain.TrackedAction {
	taken, kept := takeOutboxBlockedBy(r.outbox, key)
	if len(taken) == 0 {
		return nil
	}
	r.outbox = kept

	return taken
}
