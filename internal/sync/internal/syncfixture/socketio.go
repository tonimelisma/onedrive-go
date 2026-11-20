package syncfixture

import (
	"context"
	"sync"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

type MockSocketIOEndpointFetcher struct {
	mu        sync.Mutex
	Endpoints []*graph.SocketIOEndpoint
	calls     int
}

func (m *MockSocketIOEndpointFetcher) SocketIOEndpoint(_ context.Context, _ driveid.ID) (*graph.SocketIOEndpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls++
	if len(m.Endpoints) == 0 {
		return nil, assert.AnError
	}

	idx := m.calls - 1
	if idx >= len(m.Endpoints) {
		idx = len(m.Endpoints) - 1
	}

	return m.Endpoints[idx], nil
}

func (m *MockSocketIOEndpointFetcher) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.calls
}

// ScriptedSleepClock is wall time whose Sleep runs a test script instead of
// waiting, for tests that end or count the source's backoff sleeps.
type ScriptedSleepClock struct {
	syncdomain.SyncClock
	SleepFunc func(context.Context, time.Duration) error
}

func (c *ScriptedSleepClock) Sleep(ctx context.Context, delay time.Duration) error {
	return c.SleepFunc(ctx, delay)
}
