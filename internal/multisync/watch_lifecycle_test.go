package multisync

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"
)

func nextLifecycleEvent(t *testing.T, events <-chan WatchLifecycleEvent) WatchLifecycleEvent {
	t.Helper()

	return receiveWithin(t, events, "the next watch lifecycle event")
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
			runWatchFn: func(ctx context.Context, _ syncdomain.SyncMode, _ syncengine.WatchOptions) error {
				<-ctx.Done()
				return ctx.Err()
			},
		}, nil
	}

	cancel, done := runWatchInBackground(t, orch)

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
	assert.Equal(t, ReloadApplied, reloaded.Reload)
	require.NoError(t, reloaded.Err)
	assert.Equal(t, []string{syncRoot}, reloaded.SyncRoots)

	// A reload that cannot apply still finishes, with the error.
	require.NoError(t, os.WriteFile(cfgPath, []byte("{{invalid toml"), 0o600))
	statusCode, response := postControlReloadAnyStatus(t, cfg.ControlSocketPath)
	require.Equal(t, http.StatusUnprocessableEntity, statusCode)
	require.Equal(t, synccontrol.ErrorReloadFailed, response.Code)
	assert.Equal(t, WatchReloading, nextLifecycleEvent(t, events).Phase)
	failed := nextLifecycleEvent(t, events)
	assert.Equal(t, WatchReloaded, failed.Phase)
	assert.Equal(t, ReloadKeptPreviousSettings, failed.Reload, "the outcome is stated, not left to be read from Err")
	require.Error(t, failed.Err)
	assert.Empty(t, failed.SyncRoots)

	cancel()
	assert.Equal(t, WatchStopping, nextLifecycleEvent(t, events).Phase)

	require.NoError(t, receiveWithin(t, done, "RunWatch to return after cancellation"))
}

// Validates: R-6.6.32
func TestRunWatch_StartupFailureIsNotReady(t *testing.T) {
	// A real startup failure, not a paused drive: a selection in which every
	// drive is paused starts and idles as ready (R-3.4.2,
	// TestOrchestrator_RunWatch_IdlesWhenAllMountsPausedUntilOneIsResumed),
	// so a paused drive cannot stand in for one that never becomes ready.
	// runWithin turns a watch that idles instead of failing into a test
	// failure rather than a hang.
	rd := testStandaloneMount(t, "personal:neverready@example.com", "NeverReady")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	var phases []WatchLifecyclePhase
	cfg.WatchLifecycle = func(event WatchLifecycleEvent) { phases = append(phases, event.Phase) }

	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(_ context.Context, _ engineFactoryRequest) (engineRunner, error) {
		return nil, &syncstore.StateStoreIncompatibleError{Reason: syncstore.StateStoreIncompatibleReasonIncompatibleSchema}
	}

	err := runWithin(t, t.Context(), "a watch whose only drive failed to start to return, not idle", runWatchOf(orch))
	require.Error(t, err)
	var startupErr *WatchStartupError
	require.ErrorAs(t, err, &startupErr)
	assert.Empty(t, phases, "a watch that never started reports neither ready nor stopping")
}
