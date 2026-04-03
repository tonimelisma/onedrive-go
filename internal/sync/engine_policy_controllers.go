package sync

// scopeController owns scope lifecycle policy for one sync run. It operates on
// run-scoped engineFlow state instead of hanging that policy directly on
// engineFlow itself.
type scopeController struct {
	flow *engineFlow
}

// shortcutCoordinator owns shortcut registration, observation, and scope
// cleanup policy for one sync run.
type shortcutCoordinator struct {
	flow *engineFlow
}

func (flow *engineFlow) scopeController() *scopeController {
	return &scopeController{flow: flow}
}

func (flow *engineFlow) shortcutCoordinator() *shortcutCoordinator {
	return &shortcutCoordinator{flow: flow}
}
