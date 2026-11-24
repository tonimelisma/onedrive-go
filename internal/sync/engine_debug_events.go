package sync

import (
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

const (
	engineDebugNoteRemoteCurrent  = "remote_current"
	engineDebugNoteLocalSkipped   = "local_skipped"
	engineDebugNotePrimaryWatch   = "primary_watch"
	engineDebugNoteMountRootWatch = "mount_root_watch"
	engineDebugNoteFullRefresh    = "full_refresh"
)

// emitDebugEvent stamps event with the engine's clock and drive and hands it
// to the installed hook, if any. Every debug event of a run, the watch
// observers' included, passes through here.
//
//nolint:gocritic // Value semantics are intentional so runtime hooks cannot mutate engine-owned events.
func (e *Engine) emitDebugEvent(event syncdomain.EngineDebugEvent) {
	if event.At.IsZero() {
		event.At = e.nowFunc()
	}
	if event.DriveID == "" && !e.driveID.IsZero() {
		event.DriveID = e.driveID.String()
	}

	if e.debugEventHook != nil {
		e.debugEventHook(event)
	}
}

// SetDebugEventHook installs the sink of the engine's debug event stream.
// Call it before a run starts; the hook is called on the goroutine that
// emits each event.
func (e *Engine) SetDebugEventHook(hook func(syncdomain.DebugEvent)) {
	e.debugEventHook = hook
}

// engineDebugObserverLocal is the local observer's name in an event's
// Observer field (syncdomain.EngineDebugObserverRemote names the remote one).
const engineDebugObserverLocal = "local"

// The engine's own debug event kinds.
const (
	engineDebugEventScopeActivated                      syncdomain.EngineDebugEventType = "scope_activated"
	engineDebugEventScopeReleased                       syncdomain.EngineDebugEventType = "scope_released"
	engineDebugEventScopeDiscarded                      syncdomain.EngineDebugEventType = "scope_discarded"
	engineDebugEventScopeBatchReleased                  syncdomain.EngineDebugEventType = "scope_batch_released"
	engineDebugEventAdmittedWorkRetired                 syncdomain.EngineDebugEventType = "admitted_work_retired"
	engineDebugEventColdStartAllowanceRaised            syncdomain.EngineDebugEventType = "cold_start_allowance_raised"
	engineDebugEventColdStartHandedOver                 syncdomain.EngineDebugEventType = "cold_start_handed_over"
	engineDebugEventActionDispatched                    syncdomain.EngineDebugEventType = "action_dispatched"
	engineDebugEventRetryKicked                         syncdomain.EngineDebugEventType = "retry_kicked"
	engineDebugEventStartupScopeNormalized              syncdomain.EngineDebugEventType = "startup_scope_normalized"
	engineDebugEventBootstrapQuiesced                   syncdomain.EngineDebugEventType = "bootstrap_quiesced"
	engineDebugEventObserverStarted                     syncdomain.EngineDebugEventType = "observer_started"
	engineDebugEventObserverExited                      syncdomain.EngineDebugEventType = "observer_exited"
	engineDebugEventObserverFallbackStarted             syncdomain.EngineDebugEventType = "observer_fallback_started"
	engineDebugEventObserverFallbackStopped             syncdomain.EngineDebugEventType = "observer_fallback_stopped"
	engineDebugEventObserverFallbackScanned             syncdomain.EngineDebugEventType = "observer_fallback_scanned"
	engineDebugEventWatchStopped                        syncdomain.EngineDebugEventType = "watch_stopped"
	engineDebugEventShutdownStarted                     syncdomain.EngineDebugEventType = "shutdown_started"
	engineDebugEventRetryTimerArmed                     syncdomain.EngineDebugEventType = "retry_timer_armed"
	engineDebugEventRetryTimerFired                     syncdomain.EngineDebugEventType = "retry_timer_fired"
	engineDebugEventRetryHeldReleaseStarted             syncdomain.EngineDebugEventType = "retry_held_release_started"
	engineDebugEventRetryHeldReleaseCompleted           syncdomain.EngineDebugEventType = "retry_held_release_completed"
	engineDebugEventTrialTimerArmed                     syncdomain.EngineDebugEventType = "trial_timer_armed"
	engineDebugEventTrialTimerFired                     syncdomain.EngineDebugEventType = "trial_timer_fired"
	engineDebugEventTrialHeldReleaseStarted             syncdomain.EngineDebugEventType = "trial_held_release_started"
	engineDebugEventTrialHeldReleaseCompleted           syncdomain.EngineDebugEventType = "trial_held_release_completed"
	engineDebugEventMaintenanceTickHandled              syncdomain.EngineDebugEventType = "maintenance_tick_handled"
	engineDebugEventObservationFindingsReconcileStarted syncdomain.EngineDebugEventType = "observation_findings_reconcile_started"
	engineDebugEventSteadyStateObservationCompleted     syncdomain.EngineDebugEventType = "steady_state_observation_completed"
	engineDebugEventReadyFrontierAppended               syncdomain.EngineDebugEventType = "ready_frontier_appended"
	engineDebugEventPendingReplanSet                    syncdomain.EngineDebugEventType = "pending_replan_set"
	engineDebugEventDispatchPausedForReplan             syncdomain.EngineDebugEventType = "dispatch_paused_for_replan"
	engineDebugEventOldOutboxRetired                    syncdomain.EngineDebugEventType = "old_outbox_retired"
	engineDebugEventWaitingForRunningActions            syncdomain.EngineDebugEventType = "waiting_for_running_actions"
	engineDebugEventRunningActionsDrained               syncdomain.EngineDebugEventType = "running_actions_drained"
	engineDebugEventSteadyStateReplanStarted            syncdomain.EngineDebugEventType = "steady_state_replan_started"
	engineDebugEventLocalTruthRefreshStarted            syncdomain.EngineDebugEventType = "local_truth_refresh_started"
	engineDebugEventLocalTruthRefreshFinished           syncdomain.EngineDebugEventType = "local_truth_refresh_finished"
	engineDebugEventPlanningStarted                     syncdomain.EngineDebugEventType = "planning_started"
	engineDebugEventPlanningFinished                    syncdomain.EngineDebugEventType = "planning_finished"
	engineDebugEventNewPlanInstalled                    syncdomain.EngineDebugEventType = "new_plan_installed"
	engineDebugEventFirstPostReplanDispatch             syncdomain.EngineDebugEventType = "first_post_replan_dispatch"
	engineDebugEventRemoteRefreshStarted                syncdomain.EngineDebugEventType = "remote_refresh_started"
	engineDebugEventRemoteRefreshCommitted              syncdomain.EngineDebugEventType = "remote_refresh_committed"
	engineDebugEventRemoteRefreshApplied                syncdomain.EngineDebugEventType = "remote_refresh_applied"
	engineDebugEventRemoteRefreshDroppedOnShutdown      syncdomain.EngineDebugEventType = "remote_refresh_dropped_on_shutdown"
	engineDebugEventWebsocketWakeSourceStarted          syncdomain.EngineDebugEventType = "websocket_wake_source_started"
	engineDebugEventWebsocketEndpointFetchFail          syncdomain.EngineDebugEventType = "websocket_endpoint_fetch_failed"
	engineDebugEventWebsocketConnectFail                syncdomain.EngineDebugEventType = "websocket_connect_failed"
	engineDebugEventWebsocketConnected                  syncdomain.EngineDebugEventType = "websocket_connected"
	engineDebugEventWebsocketRefreshRequested           syncdomain.EngineDebugEventType = "websocket_refresh_requested"
	engineDebugEventWebsocketConnectionDropped          syncdomain.EngineDebugEventType = "websocket_connection_dropped"
	engineDebugEventWebsocketNotificationWake           syncdomain.EngineDebugEventType = "websocket_notification_wake"
	engineDebugEventWebsocketWakeCoalesced              syncdomain.EngineDebugEventType = "websocket_wake_coalesced"
	engineDebugEventWebsocketWakeSourceStopped          syncdomain.EngineDebugEventType = "websocket_wake_source_stopped"
	engineDebugEventWebsocketFallback                   syncdomain.EngineDebugEventType = "websocket_fallback"
)
