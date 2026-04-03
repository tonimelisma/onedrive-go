package sync

import "github.com/tonimelisma/onedrive-go/internal/synctypes"

type engineDebugEventType string

const (
	engineDebugEventScopeActivated       engineDebugEventType = "scope_activated"
	engineDebugEventScopeReleased        engineDebugEventType = "scope_released"
	engineDebugEventScopeDiscarded       engineDebugEventType = "scope_discarded"
	engineDebugEventRetryKicked          engineDebugEventType = "retry_kicked"
	engineDebugEventTrialDispatched      engineDebugEventType = "trial_dispatched"
	engineDebugEventStartupScopeRepaired engineDebugEventType = "startup_scope_repaired"
	engineDebugEventBootstrapQuiesced    engineDebugEventType = "bootstrap_quiesced"
	engineDebugEventObserverStarted      engineDebugEventType = "observer_started"
	engineDebugEventObserverExited       engineDebugEventType = "observer_exited"
	engineDebugEventWatchStopped         engineDebugEventType = "watch_stopped"
)

const (
	engineDebugObserverLocal  = "local"
	engineDebugObserverRemote = "remote"
)

type engineDebugEvent struct {
	Type     engineDebugEventType
	ScopeKey synctypes.ScopeKey
	Path     string
	Note     string
}

func (e *Engine) emitDebugEvent(event engineDebugEvent) {
	if e.debugEventHook != nil {
		e.debugEventHook(event)
	}
}
