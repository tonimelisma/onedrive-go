package sync

import (
	"fmt"
)

// watchPhase is the watch lifecycle. The runtime moves between phases only
// through transition, which reads one table; a pair the table does not list
// is an error naming both, so no call in the wrong place can leave draining
// and reopen admission during a stop.
type watchPhase uint8

const (
	// watchPhaseStarting: constructed; start-up, infrastructure and the
	// bootstrap plan run here.
	watchPhaseStarting watchPhase = iota
	// watchPhaseBootstrap: bootstrap actions draining to quiescence.
	watchPhaseBootstrap
	// watchPhaseReady: bootstrap done, observers not yet started.
	watchPhaseReady
	// watchPhaseRunning: observers live.
	watchPhaseRunning
	// watchPhaseDraining: admission sealed, completions still recorded.
	watchPhaseDraining
	// watchPhaseStopped: the drain has ended.
	watchPhaseStopped

	watchPhaseCount
)

func (p watchPhase) String() string {
	switch p {
	case watchPhaseStarting:
		return "starting"
	case watchPhaseBootstrap:
		return "bootstrap"
	case watchPhaseReady:
		return "ready"
	case watchPhaseRunning:
		return "running"
	case watchPhaseDraining:
		return "draining"
	case watchPhaseStopped:
		return "stopped"
	case watchPhaseCount:
	}

	return fmt.Sprintf("watchPhase(%d)", uint8(p))
}

// watchEvent is what moves the watch lifecycle.
type watchEvent uint8

const (
	// watchEventBootstrapPlanned: a bootstrap plan with actions is installed.
	watchEventBootstrapPlanned watchEvent = iota
	// watchEventBootstrapSettled: bootstrap quiesced, or had no actions.
	watchEventBootstrapSettled
	// watchEventObserversStarted: the observers are about to run.
	watchEventObserversStarted
	// watchEventShutdownRequested: the context ended, or a step failed
	// because it did.
	watchEventShutdownRequested
	// watchEventDrained: completions closed, no refresh, no observers.
	watchEventDrained

	watchEventCount
)

func (e watchEvent) String() string {
	switch e {
	case watchEventBootstrapPlanned:
		return "bootstrap-planned"
	case watchEventBootstrapSettled:
		return "bootstrap-settled"
	case watchEventObserversStarted:
		return "observers-started"
	case watchEventShutdownRequested:
		return "shutdown-requested"
	case watchEventDrained:
		return "drained"
	case watchEventCount:
	}

	return fmt.Sprintf("watchEvent(%d)", uint8(e))
}

// watchTransitions is the whole lifecycle. An absent pair is refused.
var watchTransitions = map[watchPhase]map[watchEvent]watchPhase{ //nolint:gochecknoglobals // the fixed lifecycle table, never written
	watchPhaseStarting: {
		watchEventBootstrapPlanned:  watchPhaseBootstrap,
		watchEventBootstrapSettled:  watchPhaseReady,
		watchEventShutdownRequested: watchPhaseDraining,
	},
	watchPhaseBootstrap: {
		watchEventBootstrapSettled:  watchPhaseReady,
		watchEventShutdownRequested: watchPhaseDraining,
	},
	watchPhaseReady: {
		watchEventObserversStarted:  watchPhaseRunning,
		watchEventShutdownRequested: watchPhaseDraining,
	},
	watchPhaseRunning: {
		watchEventShutdownRequested: watchPhaseDraining,
	},
	watchPhaseDraining: {
		watchEventShutdownRequested: watchPhaseDraining,
		watchEventDrained:           watchPhaseStopped,
	},
	watchPhaseStopped: {
		watchEventShutdownRequested: watchPhaseStopped,
	},
}

// nextWatchPhase looks the pair up in the table.
func nextWatchPhase(from watchPhase, ev watchEvent) (watchPhase, error) {
	if next, ok := watchTransitions[from][ev]; ok {
		return next, nil
	}

	return from, fmt.Errorf("sync: watch event %s is not allowed in phase %s", ev, from)
}

// transition applies ev to the runtime's phase.
func (rt *watchRuntime) transition(ev watchEvent) error {
	next, err := nextWatchPhase(rt.loop.phase, ev)
	if err != nil {
		return err
	}
	rt.loop.phase = next

	return nil
}

// watchInput names one source the watch select reads.
type watchInput uint8

const (
	watchInputDispatch watchInput = iota
	watchInputReplanReady
	watchInputCompletions
	watchInputObservations // local batches, protected-root events, remote batches
	watchInputRefreshTick
	watchInputRefreshResults
	watchInputMaintenance
	watchInputObserverErrs
	watchInputHeldTimers // trial, retry and admit timers
	watchInputBootstrapLog
	watchInputCancel
	watchInputHeldDeleteConfirm // a user's confirmation of held deletes
)

// watchInputsEnabled says which inputs each phase reads; it replaces turning
// inputs off by assigning nil to their channels. The draining row lists what
// the drain step reads; observer errors there are read while observers remain.
var watchInputsEnabled = map[watchPhase][]watchInput{ //nolint:gochecknoglobals // the fixed per-phase input table, never written
	watchPhaseBootstrap: {
		watchInputDispatch, watchInputReplanReady, watchInputCompletions,
		watchInputRefreshResults, watchInputMaintenance, watchInputHeldTimers,
		watchInputBootstrapLog, watchInputCancel,
	},
	watchPhaseRunning: {
		watchInputDispatch, watchInputReplanReady, watchInputCompletions,
		watchInputObservations, watchInputRefreshTick, watchInputRefreshResults,
		watchInputMaintenance, watchInputObserverErrs, watchInputHeldTimers,
		watchInputCancel, watchInputHeldDeleteConfirm,
	},
	watchPhaseDraining: {
		watchInputCompletions, watchInputRefreshResults, watchInputObserverErrs,
	},
}

// watchInputOn reports whether phase reads in.
func watchInputOn(phase watchPhase, in watchInput) bool {
	for _, enabled := range watchInputsEnabled[phase] {
		if enabled == in {
			return true
		}
	}

	return false
}

// recvIf returns ch when on and nil otherwise, so a select case on the result
// is live only in the phases that enable it.
func recvIf[T any](on bool, ch <-chan T) <-chan T {
	if !on {
		return nil
	}

	return ch
}
