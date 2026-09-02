package sync

import (
	"log/slog"
	"time"
)

// runtimeDeps are the capabilities every per-run flow needs regardless of what
// it is deciding: somewhere to log, the durable store, the clock, and the debug
// event stream.
//
// They were reached through a back-pointer to the whole Engine. That pointer
// carried 320 references, and four targets accounted for 191 of them -- logger,
// baseline, emitDebugEvent, and the clock. Those four are not collaborators the
// flow coordinates with; they are ambient capabilities that happened to be in
// scope. Passing them in narrows what a flow can reach for, and leaves the
// remaining engine references as what they actually are: the specific
// configuration and collaborators a particular decision consults.
type runtimeDeps struct {
	logger *slog.Logger
	store  *SyncStore
	clock  syncClock
	events func(engineDebugEvent)
}

func newRuntimeDeps(engine *Engine) runtimeDeps {
	return runtimeDeps{
		logger: engine.logger,
		store:  engine.baseline,
		clock:  engine.clock,
		events: engine.emitDebugEvent,
	}
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
func (d runtimeDeps) emit(event engineDebugEvent) {
	if d.events == nil {
		return
	}

	d.events(event)
}
