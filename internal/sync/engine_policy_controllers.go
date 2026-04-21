package sync

// scopeController owns scope lifecycle policy for one sync run. It operates on
// run-scoped engineFlow state instead of hanging that policy directly on
// engineFlow itself.
type scopeController struct {
	flow *engineFlow
}

func (flow *engineFlow) initPolicyControllers() {
	flow.scopeCtrl.flow = flow
}

func (flow *engineFlow) scopeController() *scopeController {
	return &flow.scopeCtrl
}
