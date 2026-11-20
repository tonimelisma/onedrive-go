package sync

// The engine's own debug event kinds. The stream's vocabulary, the
// observation events among it, is in domain_debug_events.go.
const (
	EngineDebugEventScopeActivated                      EngineDebugEventType = "scope_activated"
	EngineDebugEventScopeReleased                       EngineDebugEventType = "scope_released"
	EngineDebugEventScopeDiscarded                      EngineDebugEventType = "scope_discarded"
	EngineDebugEventScopeBatchReleased                  EngineDebugEventType = "scope_batch_released"
	EngineDebugEventAdmittedWorkRetired                 EngineDebugEventType = "admitted_work_retired"
	EngineDebugEventColdStartAllowanceRaised            EngineDebugEventType = "cold_start_allowance_raised"
	EngineDebugEventColdStartHandedOver                 EngineDebugEventType = "cold_start_handed_over"
	EngineDebugEventActionDispatched                    EngineDebugEventType = "action_dispatched"
	EngineDebugEventRetryKicked                         EngineDebugEventType = "retry_kicked"
	EngineDebugEventStartupScopeNormalized              EngineDebugEventType = "startup_scope_normalized"
	EngineDebugEventBootstrapQuiesced                   EngineDebugEventType = "bootstrap_quiesced"
	EngineDebugEventObserverStarted                     EngineDebugEventType = "observer_started"
	EngineDebugEventObserverExited                      EngineDebugEventType = "observer_exited"
	EngineDebugEventObserverFallbackStarted             EngineDebugEventType = "observer_fallback_started"
	EngineDebugEventObserverFallbackStopped             EngineDebugEventType = "observer_fallback_stopped"
	EngineDebugEventObserverFallbackScanned             EngineDebugEventType = "observer_fallback_scanned"
	EngineDebugEventWatchStopped                        EngineDebugEventType = "watch_stopped"
	EngineDebugEventShutdownStarted                     EngineDebugEventType = "shutdown_started"
	EngineDebugEventRetryTimerArmed                     EngineDebugEventType = "retry_timer_armed"
	EngineDebugEventRetryTimerFired                     EngineDebugEventType = "retry_timer_fired"
	EngineDebugEventRetryHeldReleaseStarted             EngineDebugEventType = "retry_held_release_started"
	EngineDebugEventRetryHeldReleaseCompleted           EngineDebugEventType = "retry_held_release_completed"
	EngineDebugEventTrialTimerArmed                     EngineDebugEventType = "trial_timer_armed"
	EngineDebugEventTrialTimerFired                     EngineDebugEventType = "trial_timer_fired"
	EngineDebugEventTrialHeldReleaseStarted             EngineDebugEventType = "trial_held_release_started"
	EngineDebugEventTrialHeldReleaseCompleted           EngineDebugEventType = "trial_held_release_completed"
	EngineDebugEventMaintenanceTickHandled              EngineDebugEventType = "maintenance_tick_handled"
	EngineDebugEventObservationFindingsReconcileStarted EngineDebugEventType = "observation_findings_reconcile_started"
	EngineDebugEventSteadyStateObservationCompleted     EngineDebugEventType = "steady_state_observation_completed"
	EngineDebugEventReadyFrontierAppended               EngineDebugEventType = "ready_frontier_appended"
	EngineDebugEventPendingReplanSet                    EngineDebugEventType = "pending_replan_set"
	EngineDebugEventDispatchPausedForReplan             EngineDebugEventType = "dispatch_paused_for_replan"
	EngineDebugEventOldOutboxRetired                    EngineDebugEventType = "old_outbox_retired"
	EngineDebugEventWaitingForRunningActions            EngineDebugEventType = "waiting_for_running_actions"
	EngineDebugEventRunningActionsDrained               EngineDebugEventType = "running_actions_drained"
	EngineDebugEventSteadyStateReplanStarted            EngineDebugEventType = "steady_state_replan_started"
	EngineDebugEventLocalTruthRefreshStarted            EngineDebugEventType = "local_truth_refresh_started"
	EngineDebugEventLocalTruthRefreshFinished           EngineDebugEventType = "local_truth_refresh_finished"
	EngineDebugEventPlanningStarted                     EngineDebugEventType = "planning_started"
	EngineDebugEventPlanningFinished                    EngineDebugEventType = "planning_finished"
	EngineDebugEventNewPlanInstalled                    EngineDebugEventType = "new_plan_installed"
	EngineDebugEventFirstPostReplanDispatch             EngineDebugEventType = "first_post_replan_dispatch"
	EngineDebugEventRemoteRefreshStarted                EngineDebugEventType = "remote_refresh_started"
	EngineDebugEventRemoteRefreshCommitted              EngineDebugEventType = "remote_refresh_committed"
	EngineDebugEventRemoteRefreshApplied                EngineDebugEventType = "remote_refresh_applied"
	EngineDebugEventRemoteRefreshDroppedOnShutdown      EngineDebugEventType = "remote_refresh_dropped_on_shutdown"
	EngineDebugEventWebsocketWakeSourceStarted          EngineDebugEventType = "websocket_wake_source_started"
	EngineDebugEventWebsocketEndpointFetchFail          EngineDebugEventType = "websocket_endpoint_fetch_failed"
	EngineDebugEventWebsocketConnectFail                EngineDebugEventType = "websocket_connect_failed"
	EngineDebugEventWebsocketConnected                  EngineDebugEventType = "websocket_connected"
	EngineDebugEventWebsocketRefreshRequested           EngineDebugEventType = "websocket_refresh_requested"
	EngineDebugEventWebsocketConnectionDropped          EngineDebugEventType = "websocket_connection_dropped"
	EngineDebugEventWebsocketNotificationWake           EngineDebugEventType = "websocket_notification_wake"
	EngineDebugEventWebsocketWakeCoalesced              EngineDebugEventType = "websocket_wake_coalesced"
	EngineDebugEventWebsocketWakeSourceStopped          EngineDebugEventType = "websocket_wake_source_stopped"
	EngineDebugEventWebsocketFallback                   EngineDebugEventType = "websocket_fallback"
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
func (e *Engine) emitDebugEvent(event EngineDebugEvent) {
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
