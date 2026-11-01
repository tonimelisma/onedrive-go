package multisync

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"
)

func nextLifecycleEvent(t *testing.T, events <-chan WatchLifecycleEvent) WatchLifecycleEvent {
	t.Helper()

	select {
	case event := <-events:
		return event
	case <-time.After(5 * time.Second):
		require.Fail(t, "no watch lifecycle event")
		return WatchLifecycleEvent{}
	}
}

// Validates: R-6.6.32
func TestRunWatch_ReportsLifecycleTransitions(t *testing.T) {
	rd := testStandaloneMount(t, "personal:lifecycle@example.com", "Lifecycle")
	// XDG isolation so reload mount compilation finds catalog drive identity.
	setupXDGIsolation(t, rd.CanonicalID)
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	syncRoot := cfg.StandaloneMounts[0].SyncRoot
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	events := make(chan WatchLifecycleEvent, 16)
	cfg.WatchLifecycle = func(event WatchLifecycleEvent) { events <- event }

	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(_ context.Context, _ engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
				<-ctx.Done()
				return ctx.Err()
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(ctx, syncengine.SyncBidirectional, syncengine.WatchOptions{})
	}()

	ready := nextLifecycleEvent(t, events)
	assert.Equal(t, WatchReady, ready.Phase)
	assert.Equal(t, []string{syncRoot}, ready.SyncRoots)
	assert.Empty(t, ready.Skipped)
	require.NoError(t, ready.Err)

	// A reload that applies reports Reloading, then Reloaded with the new set.
	postControlReload(t, cfg.ControlSocketPath)
	assert.Equal(t, WatchReloading, nextLifecycleEvent(t, events).Phase)
	reloaded := nextLifecycleEvent(t, events)
	assert.Equal(t, WatchReloaded, reloaded.Phase)
	require.NoError(t, reloaded.Err)
	assert.Equal(t, []string{syncRoot}, reloaded.SyncRoots)

	// A reload that cannot apply still finishes, with the error.
	require.NoError(t, os.WriteFile(cfgPath, []byte("{{invalid toml"), 0o600))
	statusCode, response := postControlJSONAnyStatus(t, cfg.ControlSocketPath, synccontrol.PathReload, nil)
	require.Equal(t, http.StatusUnprocessableEntity, statusCode)
	require.Equal(t, synccontrol.ErrorReloadFailed, response.Code)
	assert.Equal(t, WatchReloading, nextLifecycleEvent(t, events).Phase)
	failed := nextLifecycleEvent(t, events)
	assert.Equal(t, WatchReloaded, failed.Phase)
	require.Error(t, failed.Err)
	assert.Empty(t, failed.SyncRoots)

	cancel()
	assert.Equal(t, WatchStopping, nextLifecycleEvent(t, events).Phase)

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not stop in time")
	}
}

// Validates: R-6.6.32
func TestRunWatch_StartupFailureIsNotReady(t *testing.T) {
	// A real startup failure, not a paused drive: a selection in which every
	// drive is paused starts and idles as ready (R-3.4.2,
	// TestOrchestrator_RunWatch_IdlesWhenAllMountsPausedUntilOneIsResumed),
	// which this fixture once relied on to never be ready. The deadline
	// turns a watch that idles instead of failing into a test failure
	// rather than a hang.
	rd := testStandaloneMount(t, "personal:neverready@example.com", "NeverReady")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	var phases []WatchLifecyclePhase
	cfg.WatchLifecycle = func(event WatchLifecycleEvent) { phases = append(phases, event.Phase) }

	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(_ context.Context, _ engineFactoryRequest) (engineRunner, error) {
		return nil, &syncengine.StateStoreIncompatibleError{Reason: syncengine.StateStoreIncompatibleReasonIncompatibleSchema}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err := orch.RunWatch(ctx, syncengine.SyncBidirectional, syncengine.WatchOptions{})
	require.NoError(t, ctx.Err(), "a watch whose only drive failed to start must return, not idle")
	require.Error(t, err)
	var startupErr *WatchStartupError
	require.ErrorAs(t, err, &startupErr)
	assert.Empty(t, phases, "a watch that never started reports neither ready nor stopping")
}
