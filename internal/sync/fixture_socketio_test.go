package sync

import (
	"context"
	"sync"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

type mockSocketIOEndpointFetcher struct {
	mu        sync.Mutex
	endpoints []*graph.SocketIOEndpoint
	calls     int
}

func (m *mockSocketIOEndpointFetcher) SocketIOEndpoint(_ context.Context, _ driveid.ID) (*graph.SocketIOEndpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls++
	if len(m.endpoints) == 0 {
		return nil, assert.AnError
	}

	idx := m.calls - 1
	if idx >= len(m.endpoints) {
		idx = len(m.endpoints) - 1
	}

	return m.endpoints[idx], nil
}

func (m *mockSocketIOEndpointFetcher) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.calls
}

// scriptedSleepClock is wall time whose Sleep runs a test script instead of
// waiting, for tests that end or count the source's backoff sleeps.
type scriptedSleepClock struct {
	SyncClock
	sleep func(context.Context, time.Duration) error
}

func (c *scriptedSleepClock) Sleep(ctx context.Context, delay time.Duration) error {
	return c.sleep(ctx, delay)
}
