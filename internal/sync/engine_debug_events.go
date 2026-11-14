package sync

// The engine's own debug event kinds. The stream's vocabulary, the
// observation events among it, is in domain_debug_events.go.
const (
	engineDebugEventScopeActivated                      engineDebugEventType = "scope_activated"
	engineDebugEventScopeReleased                       engineDebugEventType = "scope_released"
	engineDebugEventScopeDiscarded                      engineDebugEventType = "scope_discarded"
	engineDebugEventScopeBatchReleased                  engineDebugEventType = "scope_batch_released"
	engineDebugEventAdmittedWorkRetired                 engineDebugEventType = "admitted_work_retired"
	engineDebugEventColdStartAllowanceRaised            engineDebugEventType = "cold_start_allowance_raised"
	engineDebugEventColdStartHandedOver                 engineDebugEventType = "cold_start_handed_over"
	engineDebugEventActionDispatched                    engineDebugEventType = "action_dispatched"
	engineDebugEventRetryKicked                         engineDebugEventType = "retry_kicked"
	engineDebugEventStartupScopeNormalized              engineDebugEventType = "startup_scope_normalized"
	engineDebugEventBootstrapQuiesced                   engineDebugEventType = "bootstrap_quiesced"
	engineDebugEventObserverStarted                     engineDebugEventType = "observer_started"
	engineDebugEventObserverExited                      engineDebugEventType = "observer_exited"
	engineDebugEventObserverFallbackStarted             engineDebugEventType = "observer_fallback_started"
	engineDebugEventObserverFallbackStopped             engineDebugEventType = "observer_fallback_stopped"
	engineDebugEventObserverFallbackScanned             engineDebugEventType = "observer_fallback_scanned"
	engineDebugEventWatchStopped                        engineDebugEventType = "watch_stopped"
	engineDebugEventShutdownStarted                     engineDebugEventType = "shutdown_started"
	engineDebugEventRetryTimerArmed                     engineDebugEventType = "retry_timer_armed"
	engineDebugEventRetryTimerFired                     engineDebugEventType = "retry_timer_fired"
	engineDebugEventRetryHeldReleaseStarted             engineDebugEventType = "retry_held_release_started"
	engineDebugEventRetryHeldReleaseCompleted           engineDebugEventType = "retry_held_release_completed"
	engineDebugEventTrialTimerArmed                     engineDebugEventType = "trial_timer_armed"
	engineDebugEventTrialTimerFired                     engineDebugEventType = "trial_timer_fired"
	engineDebugEventTrialHeldReleaseStarted             engineDebugEventType = "trial_held_release_started"
	engineDebugEventTrialHeldReleaseCompleted           engineDebugEventType = "trial_held_release_completed"
	engineDebugEventMaintenanceTickHandled              engineDebugEventType = "maintenance_tick_handled"
	engineDebugEventObservationFindingsReconcileStarted engineDebugEventType = "observation_findings_reconcile_started"
	engineDebugEventSteadyStateObservationCompleted     engineDebugEventType = "steady_state_observation_completed"
	engineDebugEventReadyFrontierAppended               engineDebugEventType = "ready_frontier_appended"
	engineDebugEventPendingReplanSet                    engineDebugEventType = "pending_replan_set"
	engineDebugEventDispatchPausedForReplan             engineDebugEventType = "dispatch_paused_for_replan"
	engineDebugEventOldOutboxRetired                    engineDebugEventType = "old_outbox_retired"
	engineDebugEventWaitingForRunningActions            engineDebugEventType = "waiting_for_running_actions"
	engineDebugEventRunningActionsDrained               engineDebugEventType = "running_actions_drained"
	engineDebugEventSteadyStateReplanStarted            engineDebugEventType = "steady_state_replan_started"
	engineDebugEventLocalTruthRefreshStarted            engineDebugEventType = "local_truth_refresh_started"
	engineDebugEventLocalTruthRefreshFinished           engineDebugEventType = "local_truth_refresh_finished"
	engineDebugEventPlanningStarted                     engineDebugEventType = "planning_started"
	engineDebugEventPlanningFinished                    engineDebugEventType = "planning_finished"
	engineDebugEventNewPlanInstalled                    engineDebugEventType = "new_plan_installed"
	engineDebugEventFirstPostReplanDispatch             engineDebugEventType = "first_post_replan_dispatch"
	engineDebugEventRemoteRefreshStarted                engineDebugEventType = "remote_refresh_started"
	engineDebugEventRemoteRefreshCommitted              engineDebugEventType = "remote_refresh_committed"
	engineDebugEventRemoteRefreshApplied                engineDebugEventType = "remote_refresh_applied"
	engineDebugEventRemoteRefreshDroppedOnShutdown      engineDebugEventType = "remote_refresh_dropped_on_shutdown"
	engineDebugEventWebsocketWakeSourceStarted          engineDebugEventType = "websocket_wake_source_started"
	engineDebugEventWebsocketEndpointFetchFail          engineDebugEventType = "websocket_endpoint_fetch_failed"
	engineDebugEventWebsocketConnectFail                engineDebugEventType = "websocket_connect_failed"
	engineDebugEventWebsocketConnected                  engineDebugEventType = "websocket_connected"
	engineDebugEventWebsocketRefreshRequested           engineDebugEventType = "websocket_refresh_requested"
	engineDebugEventWebsocketConnectionDropped          engineDebugEventType = "websocket_connection_dropped"
	engineDebugEventWebsocketNotificationWake           engineDebugEventType = "websocket_notification_wake"
	engineDebugEventWebsocketWakeCoalesced              engineDebugEventType = "websocket_wake_coalesced"
	engineDebugEventWebsocketWakeSourceStopped          engineDebugEventType = "websocket_wake_source_stopped"
	engineDebugEventWebsocketFallback                   engineDebugEventType = "websocket_fallback"
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
func (e *Engine) emitDebugEvent(event engineDebugEvent) {
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
func (e *Engine) SetDebugEventHook(hook func(DebugEvent)) {
	e.debugEventHook = hook
}
