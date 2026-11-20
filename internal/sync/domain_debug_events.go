package sync

import "time"

// The vocabulary of the engine's debug event stream: diagnostics for tests
// and traces, which never own or redirect control flow. It sits below every
// family because the watch observers report into the stream as well as the
// engine; the engine owns the sink (Engine.SetDebugEventHook) and hands it to
// the observers it starts (engine_debug_events.go). The engine's own event
// kinds follow the observation events.

type EngineDebugEventType string

// The observation events: inputs a watch observer dropped by decision. Each
// is the observer's report that it handled the input, so a test waits for it
// instead of waiting out a window in which nothing should happen.
const (
	// EngineDebugEventLocalCaseCollisionSkipped: the local observer skipped
	// an entry whose name OneDrive treats as a sibling's (R-2.12.2): a watch
	// create, an entry of a new directory's scan, or the hash of a coalesced
	// write. Path is the skipped entry, Note the name it collides with.
	EngineDebugEventLocalCaseCollisionSkipped EngineDebugEventType = "local_case_collision_skipped"
	// EngineDebugEventRemoteWakesCoalesced: the remote observer dropped Count
	// wake signals that were waiting when a poll began; that poll covers them.
	EngineDebugEventRemoteWakesCoalesced EngineDebugEventType = "remote_wakes_coalesced"
)

// The observers an event's Observer field names.
const (
	EngineDebugObserverLocal  = "local"
	EngineDebugObserverRemote = "remote"
)

type EngineDebugEvent struct {
	At          time.Time
	Type        EngineDebugEventType
	DriveID     string
	ScopeKey    ScopeKey
	Path        string
	Observer    string
	Delay       time.Duration
	Note        string
	Count       int
	Outbox      int
	Running     int
	IdleWorkers int
	Error       string
}

// DebugEvent is one event of the engine's debug stream, as a hook installed
// with Engine.SetDebugEventHook receives it.
type DebugEvent = EngineDebugEvent

// The engine's own debug event kinds.
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
