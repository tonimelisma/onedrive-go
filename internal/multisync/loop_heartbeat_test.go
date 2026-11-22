package multisync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

func TestLoopHeartbeat_DeclaredStepExtendsTheLimit(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	h := newLoopHeartbeat(clock.now)

	clock.advance(loopSilenceLimit)
	ok, _, _ := h.alive()
	assert.True(t, ok, "silent exactly the limit is still alive")
	clock.advance(time.Second)
	ok, _, silent := h.alive()
	assert.False(t, ok)
	assert.Equal(t, loopSilenceLimit+time.Second, silent)

	h.beatFor("stopping a drive", 3*loopSilenceLimit)
	clock.advance(2 * loopSilenceLimit)
	ok, step, _ := h.alive()
	assert.True(t, ok, "a declared step holds the loop up to its own bound")
	assert.Equal(t, "stopping a drive", step)
	clock.advance(loopSilenceLimit + time.Second)
	ok, _, _ = h.alive()
	assert.False(t, ok, "past the declared bound the loop counts as hung")

	h.beat()
	ok, step, silent = h.alive()
	assert.True(t, ok)
	assert.Empty(t, step)
	assert.Zero(t, silent)

	h.beatFor("short", time.Millisecond)
	clock.advance(loopSilenceLimit)
	ok, _, _ = h.alive()
	assert.True(t, ok, "a declared step never shortens the ordinary limit")
}

func TestRunWatch_BeatsOnEveryIterationAndWhileIdle(t *testing.T) {
	rd := testStandaloneMount(t, "personal:heartbeat@example.com", "Heartbeat")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)
	clock := newTestClock()
	orch.now = clock.now
	orch.reconcileTicks = func(time.Duration) (<-chan time.Time, func()) { return nil, func() {} }
	beats := make(chan time.Time)
	var interval time.Duration
	orch.heartbeatTicks = func(d time.Duration) (<-chan time.Time, func()) {
		interval = d
		return beats, func() {}
	}
	orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncdomain.SyncMode, _ syncengine.WatchOptions) error {
				<-ctx.Done()
				return ctx.Err()
			},
		}, nil
	}

	cancel, done := runWatchInBackground(t, orch)
	defer stopWatch(t, cancel, done)

	// The loop is idle; each tick it takes is a beat at the top of the next
	// iteration, so a clock advanced past the silence limit between ticks
	// still reads alive once the tick has been taken.
	for range 3 {
		clock.advance(loopSilenceLimit + time.Second)
		select {
		case beats <- clock.now():
		case <-time.After(5 * time.Second):
			require.FailNow(t, "the watch loop did not take its heartbeat tick")
		}
		// The send hands the tick over; the next send can happen only
		// after the loop has gone round and beaten, which the next
		// iteration of this loop waits for.
	}
	select {
	case beats <- clock.now():
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the watch loop did not take its heartbeat tick")
	}
	require.Eventually(t, func() bool {
		ok, step, _ := orch.LoopAlive()
		return ok && step == ""
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, loopHeartbeatInterval, interval)

	clock.advance(loopSilenceLimit + time.Second)
	ok, _, _ := orch.LoopAlive()
	assert.False(t, ok, "a loop that takes no tick goes silent")
}
