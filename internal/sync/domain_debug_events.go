package sync

import "time"

// The vocabulary of the engine's debug event stream: diagnostics for tests
// and traces, which never own or redirect control flow. It sits below every
// family because the watch observers report into the stream as well as the
// engine; the engine owns the sink (Engine.SetDebugEventHook) and hands it to
// the observers it starts. The engine's own event kinds are in
// engine_debug_events.go.

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
