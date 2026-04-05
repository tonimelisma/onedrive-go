package sync

import (
	"time"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type engineDebugEventType string

const (
	engineDebugEventScopeActivated             engineDebugEventType = "scope_activated"
	engineDebugEventScopeReleased              engineDebugEventType = "scope_released"
	engineDebugEventScopeDiscarded             engineDebugEventType = "scope_discarded"
	engineDebugEventRetryKicked                engineDebugEventType = "retry_kicked"
	engineDebugEventTrialDispatched            engineDebugEventType = "trial_dispatched"
	engineDebugEventStartupScopeRepaired       engineDebugEventType = "startup_scope_repaired"
	engineDebugEventBootstrapQuiesced          engineDebugEventType = "bootstrap_quiesced"
	engineDebugEventObserverStarted            engineDebugEventType = "observer_started"
	engineDebugEventObserverExited             engineDebugEventType = "observer_exited"
	engineDebugEventObserverFallbackStarted    engineDebugEventType = "observer_fallback_started"
	engineDebugEventObserverFallbackStopped    engineDebugEventType = "observer_fallback_stopped"
	engineDebugEventWatchStopped               engineDebugEventType = "watch_stopped"
	engineDebugEventShutdownStarted            engineDebugEventType = "shutdown_started"
	engineDebugEventRetryTimerArmed            engineDebugEventType = "retry_timer_armed"
	engineDebugEventRetryTimerFired            engineDebugEventType = "retry_timer_fired"
	engineDebugEventRetrySweepStarted          engineDebugEventType = "retry_sweep_started"
	engineDebugEventRetrySweepCompleted        engineDebugEventType = "retry_sweep_completed"
	engineDebugEventTrialTimerArmed            engineDebugEventType = "trial_timer_armed"
	engineDebugEventTrialTimerFired            engineDebugEventType = "trial_timer_fired"
	engineDebugEventTrialSweepStarted          engineDebugEventType = "trial_sweep_started"
	engineDebugEventTrialSweepCompleted        engineDebugEventType = "trial_sweep_completed"
	engineDebugEventRecheckTickHandled         engineDebugEventType = "recheck_tick_handled"
	engineDebugEventReconcileStarted           engineDebugEventType = "reconcile_started"
	engineDebugEventReconcileApplied           engineDebugEventType = "reconcile_applied"
	engineDebugEventReconcileDroppedOnShutdown engineDebugEventType = "reconcile_dropped_on_shutdown"
)

const (
	engineDebugObserverLocal  = "local"
	engineDebugObserverRemote = "remote"
)

type engineDebugEvent struct {
	Type     engineDebugEventType
	ScopeKey synctypes.ScopeKey
	Path     string
	Observer string
	Delay    time.Duration
	Note     string
	Count    int
}

func (e *Engine) emitDebugEvent(event engineDebugEvent) {
	if e.debugEventHook != nil {
		e.debugEventHook(event)
	}
}
