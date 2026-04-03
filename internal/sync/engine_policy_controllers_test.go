package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEngineFlow_PolicyControllersAreRunOwned(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	flow := newEngineFlow(eng.Engine)

	scopeCtrl := flow.scopeController()
	shortcutCtrl := flow.shortcutCoordinator()

	assert.Same(t, &flow.scopeCtrl, scopeCtrl)
	assert.Same(t, &flow.shortcutCtrl, shortcutCtrl)
	assert.Same(t, &flow, scopeCtrl.flow)
	assert.Same(t, &flow, shortcutCtrl.flow)
}

func TestEngineFlow_PolicyControllersRebindAfterValueCopy(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	original := newEngineFlow(eng.Engine)
	copied := original

	scopeCtrl := copied.scopeController()
	shortcutCtrl := copied.shortcutCoordinator()

	assert.Same(t, &copied.scopeCtrl, scopeCtrl)
	assert.Same(t, &copied.shortcutCtrl, shortcutCtrl)
	assert.Same(t, &copied, scopeCtrl.flow)
	assert.Same(t, &copied, shortcutCtrl.flow)
	assert.NotSame(t, &original, scopeCtrl.flow)
	assert.NotSame(t, &original, shortcutCtrl.flow)
}
