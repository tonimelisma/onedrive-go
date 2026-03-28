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
