package syncdomain

import "time"

// The vocabulary of the engine's debug event stream: diagnostics for tests
// and traces, which never own or redirect control flow. It sits below every
// family because the watch observers report into the stream as well as the
// engine; the engine owns the sink (Engine.SetDebugEventHook) and hands it to
// the observers it starts (engine_debug_events.go). The engine's own event
// kinds, and the name of the local observer it runs itself, are the
// engine's (internal/sync's engine_debug_events.go).

type EngineDebugEventType string

// The observation events: inputs a watch observer dropped by decision. Each
// is the observer's report that it handled the input, so a test waits for it
// instead of waiting out a window in which nothing should happen.
const (
	// EngineDebugEventRemoteWakesCoalesced: the remote observer dropped Count
	// wake signals that were waiting when a poll began; that poll covers them.
	EngineDebugEventRemoteWakesCoalesced EngineDebugEventType = "remote_wakes_coalesced"
)

// EngineDebugObserverRemote is the remote observer's name in an event's
// Observer field.
const EngineDebugObserverRemote = "remote"

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
