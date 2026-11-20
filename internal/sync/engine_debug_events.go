package sync

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
