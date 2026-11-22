package sync

import (
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/perf"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// runtimeDeps are the capabilities every per-run flow needs regardless of what
// it is deciding: somewhere to log, the durable store, the clock, the debug
// event stream and the perf collector. They are ambient capabilities, not
// collaborators the flow coordinates with, so a flow is handed them
// (flowInputs) rather than reaching for them through the engine.
type runtimeDeps struct {
	logger *slog.Logger
	store  *syncstore.SyncStore
	clock  syncdomain.SyncClock
	events func(syncdomain.EngineDebugEvent)
	// perf is the engine's perf collector; nil records nothing.
	perf *perf.Collector
	// wall is the engine's real clock, whose timer panics end the run a
	// callback panicked in, whatever clock the run reads (clock).
	wall syncdomain.RealClock
	// activityFeed publishes a classified completion to the engine's
	// activity feed, and activityListens reports whether that feed says a
	// condition starts with a failure (Engine.emitActivity,
	// Engine.activitySaysConditionStarts); nil publishes nothing.
	activityFeed    func(*syncdomain.ActionCompletion, *syncdomain.ResultDecision, bool)
	activityListens func(*syncdomain.ResultDecision, *syncdomain.ActionCompletion, bool) bool
}

func newRuntimeDeps(engine *Engine) runtimeDeps {
	return runtimeDeps{
		logger:          engine.logger,
		store:           engine.baseline,
		clock:           engine.runClock(),
		events:          engine.emitDebugEvent,
		perf:            engine.collector(),
		wall:            engine.wallClock,
		activityFeed:    engine.emitActivity,
		activityListens: engine.activitySaysConditionStarts,
	}
}

// activity publishes a classified completion to the activity feed.
func (d runtimeDeps) activity(r *syncdomain.ActionCompletion, decision *syncdomain.ResultDecision, conditionStarts bool) {
	if d.activityFeed != nil {
		d.activityFeed(r, decision, conditionStarts)
	}
}

// activitySaysConditionStarts reports whether the activity feed tells its
// consumer that the condition that refused this failure starts with it.
func (d runtimeDeps) activitySaysConditionStarts(
	decision *syncdomain.ResultDecision, r *syncdomain.ActionCompletion, conditionStarts bool,
) bool {
	return d.activityListens != nil && d.activityListens(decision, r, conditionStarts)
}

// now reads the run's clock. Every timing decision in a run reads the same
// clock value, so a test that controls time controls all of them.
func (d runtimeDeps) now() time.Time {
	return d.clock.Now()
}

// since measures elapsed time against the run's clock rather than wall time.
func (d runtimeDeps) since(start time.Time) time.Duration {
	return d.now().Sub(start)
}

// emit publishes a debug event when a sink is installed. Debug events are
// diagnostics: they never own or redirect control flow.
//
//nolint:gocritic // Value semantics are intentional so runtime hooks cannot mutate engine-owned events, matching Engine.emitDebugEvent.
func (d runtimeDeps) emit(event syncdomain.EngineDebugEvent) {
	if d.events == nil {
		return
	}

	d.events(event)
}
