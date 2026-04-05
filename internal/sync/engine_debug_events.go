package sync

import (
	"time"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type engineDebugEventType string

type DebugEventType = engineDebugEventType

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
	engineDebugEventWebsocketWakeSourceStarted engineDebugEventType = "websocket_wake_source_started"
	engineDebugEventWebsocketEndpointFetchFail engineDebugEventType = "websocket_endpoint_fetch_failed"
	engineDebugEventWebsocketConnectFail       engineDebugEventType = "websocket_connect_failed"
	engineDebugEventWebsocketConnected         engineDebugEventType = "websocket_connected"
	engineDebugEventWebsocketRefreshRequested  engineDebugEventType = "websocket_refresh_requested"
	engineDebugEventWebsocketConnectionDropped engineDebugEventType = "websocket_connection_dropped"
	engineDebugEventWebsocketNotificationWake  engineDebugEventType = "websocket_notification_wake"
	engineDebugEventWebsocketWakeCoalesced     engineDebugEventType = "websocket_wake_coalesced"
	engineDebugEventWebsocketWakeSourceStopped engineDebugEventType = "websocket_wake_source_stopped"
	engineDebugEventWebsocketFallback          engineDebugEventType = "websocket_fallback"
)

const (
	engineDebugObserverLocal  = "local"
	engineDebugObserverRemote = "remote"
)

type engineDebugEvent struct {
	Type     engineDebugEventType
	DriveID  string
	ScopeKey synctypes.ScopeKey
	Path     string
	Observer string
	Delay    time.Duration
	Note     string
	Count    int
	Error    string
}

type DebugEvent = engineDebugEvent

//nolint:gocritic // Value semantics are intentional so runtime hooks cannot mutate engine-owned events.
func (e *Engine) emitDebugEvent(event engineDebugEvent) {
	if event.DriveID == "" && !e.driveID.IsZero() {
		event.DriveID = e.driveID.String()
	}

	if e.debugEventHook != nil {
		e.debugEventHook(event)
	}
}

func (e *Engine) SetDebugEventHook(hook func(DebugEvent)) {
	e.debugEventHook = hook
}
