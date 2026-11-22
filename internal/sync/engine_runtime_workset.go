package sync

import (
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// initializeRuntimeState installs a plan's runtime state: the retry rows it
// was planned against, and in the scheduler every per-plan structure afresh
// with the plan's active scopes.
func (flow *engineFlow) initializeRuntimeState(runtime *runtimePlan) {
	flow.retryWork.install(runtime.RetryRows)

	activeScopes := make([]syncdomain.ActiveScope, 0, len(runtime.BlockScopes))
	for i := range runtime.BlockScopes {
		if runtime.BlockScopes[i] == nil {
			continue
		}
		activeScopes = append(activeScopes, syncdomain.ActiveScopeFromBlockScopeRow(runtime.BlockScopes[i]))
	}
	flow.sched.Install(flow.cfg.poolSize, activeScopes)
	// A target already under a throttle (or service) scope when the plan
	// starts is the scope's to ramp (R-6.8.24); the slow start is for a
	// target nothing is known about (R-6.8.25).
	for i := range activeScopes {
		flow.handOverColdStart(activeScopes[i].Key)
	}
}
