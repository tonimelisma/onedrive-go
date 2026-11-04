package multisync

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// Validates: R-2.8.1
//
// A config reload hands the file's log_level to the caller's setter, so a
// level changed in config.toml reaches the log file of a running watch
// without a restart (on 2026-11-02 a debug level set for a diagnosis took
// effect only at the next restart).
func TestOrchestrator_Reload_AppliesTheConfigLogLevel(t *testing.T) {
	rd := testStandaloneMount(t, "personal:loglevel@example.com", "LogLevel")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	var applied atomic.Value
	cfg.ApplyLogLevel = func(level string) { applied.Store(level) }

	orch := NewOrchestrator(cfg)
	var started atomic.Int32
	orch.engineFactory = func(_ context.Context, _ engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
				started.Add(1)
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
	require.Eventually(t, func() bool { return started.Load() >= 1 }, 5*time.Second, 10*time.Millisecond)

	// #nosec G304 -- cfgPath is a test-owned config path from writeTestConfig.
	data, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	// #nosec G703 -- the same test-owned path.
	require.NoError(t, os.WriteFile(cfgPath, append([]byte("log_level = \"debug\"\n"), data...), 0o600))
	postControlReload(t, cfg.ControlSocketPath)

	require.Eventually(t, func() bool {
		level, ok := applied.Load().(string)

		return ok && level == "debug"
	}, 5*time.Second, 10*time.Millisecond, "the reload did not hand the new log_level over")

	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "RunWatch did not stop in time")
	}
}
