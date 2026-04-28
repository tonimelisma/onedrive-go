package multisync

import (
	"context"
	"fmt"
	"log/slog"
	gosync "sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"
)

// engineRunner is the interface the Orchestrator uses to run sync passes.
// Implemented by *sync.Engine; mock implementations are used in tests.
type engineRunner interface {
	RunOnce(ctx context.Context, mode syncengine.SyncMode, opts syncengine.RunOptions) (*syncengine.Report, error)
	RunWatch(ctx context.Context, mode syncengine.SyncMode, opts syncengine.WatchOptions) error
	Close(ctx context.Context) error
	ShortcutChildAckHandle() shortcutChildAckHandle
}

type engineRunnerAdapter struct {
	engine *syncengine.Engine
}

func (a engineRunnerAdapter) RunOnce(
	ctx context.Context,
	mode syncengine.SyncMode,
	opts syncengine.RunOptions,
) (*syncengine.Report, error) {
	report, err := a.engine.RunOnce(ctx, mode, opts)
	if err != nil {
		return report, fmt.Errorf("run sync engine once: %w", err)
	}
	return report, nil
}

func (a engineRunnerAdapter) RunWatch(
	ctx context.Context,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
) error {
	if err := a.engine.RunWatch(ctx, mode, opts); err != nil {
		return fmt.Errorf("run sync engine watch: %w", err)
	}
	return nil
}

func (a engineRunnerAdapter) Close(ctx context.Context) error {
	if err := a.engine.Close(ctx); err != nil {
		return fmt.Errorf("close sync engine: %w", err)
	}
	return nil
}

func (a engineRunnerAdapter) ShortcutChildAckHandle() shortcutChildAckHandle {
	return a.engine.ShortcutChildAckHandle()
}

func shortcutParentAckHandleForMount(mount *mountSpec, engine engineRunner) shortcutChildAckHandle {
	if mount == nil || mount.projectionKind != MountProjectionStandalone || engine == nil {
		return nil
	}
	return engine.ShortcutChildAckHandle()
}

type engineFactoryRequest struct {
	Session       *driveops.Session
	Mount         *mountSpec
	Logger        *slog.Logger
	VerifyDrive   bool
	PerfCollector *perf.Collector
}

// engineFactoryFunc creates an engineRunner from the runtime mount/session pair
// used by production orchestration. Tests inject mocks at this boundary.
type engineFactoryFunc func(ctx context.Context, req engineFactoryRequest) (engineRunner, error)

// OrchestratorConfig holds the inputs for creating an Orchestrator.
// The CLI layer populates this from standalone mount config and HTTP clients.
// Config and config path are accessed via Holder — a single source of truth
// shared with SessionRuntime. Control-socket reload updates config in one place.
type OrchestratorConfig struct {
	Holder                 *config.Holder
	StandaloneMounts       []StandaloneMountConfig
	InitialStartupResults  []MountStartupResult
	ReloadStandaloneMounts func(*config.Config) (StandaloneMountSelection, error)
	Runtime                *driveops.SessionRuntime // token caching + Session creation
	DataDir                string
	Logger                 *slog.Logger
	ControlSocketPath      string
	StartWarning           func(StartupWarning)
	DebugEventHook         func(syncengine.DebugEvent)
	PerfParent             *perf.Collector
}

// Orchestrator manages per-mount sync runners. It is always used, even for a
// single mount, so one-shot and watch mode share the same top-level lifecycle.
type Orchestrator struct {
	cfg           *OrchestratorConfig
	engineFactory engineFactoryFunc // injectable for tests
	logger        *slog.Logger
	perfRuntime   *perf.Runtime
	statusMu      gosync.RWMutex
	controlMounts []string
	// shortcutCleanupDiagnostics is transient executor status for control
	// clients. Durable recovery remains in parent shortcut_roots.
	shortcutCleanupDiagnostics []synccontrol.ShortcutCleanupDiagnostic
	shortcutMu                 gosync.Mutex
	// latestParentChildWorkSnapshots is an ephemeral exact cache of parent
	// child-work intent. It is rebuildable from live parents, is cleared when the
	// parent exits or restarts, and never owns shortcut lifecycle policy.
	latestParentChildWorkSnapshots map[mountID]syncengine.ShortcutChildWorkSnapshot
	reconcileTicks                 func(time.Duration) (<-chan time.Time, func())
	artifactCleanup                shortcutChildArtifactCleanupExecutor
}

// NewOrchestrator creates an Orchestrator with real Engine factory.
// Token/client caching is handled by the SessionRuntime in cfg.Runtime.
// Tests inject stubs via cfg.Runtime.TokenSourceFn and engineFactory.
func NewOrchestrator(cfg *OrchestratorConfig) *Orchestrator {
	return &Orchestrator{
		cfg: cfg,
		engineFactory: func(ctx context.Context, req engineFactoryRequest) (engineRunner, error) {
			mountCfg, err := engineMountConfigForMount(req.Mount, cfg.DataDir)
			if err != nil {
				return nil, fmt.Errorf("engine mount config: %w", err)
			}
			engine, err := syncengine.NewMountEngine(
				ctx,
				req.Session,
				mountCfg,
				req.Logger,
				req.PerfCollector,
				req.VerifyDrive,
			)
			if err != nil {
				return nil, fmt.Errorf("new engine: %w", err)
			}
			if cfg.DebugEventHook != nil {
				engine.SetDebugEventHook(cfg.DebugEventHook)
			}
			return engineRunnerAdapter{engine: engine}, nil
		},
		logger:                         cfg.Logger,
		perfRuntime:                    perf.NewRuntime(cfg.PerfParent),
		latestParentChildWorkSnapshots: make(map[mountID]syncengine.ShortcutChildWorkSnapshot),
		artifactCleanup:                newShortcutChildArtifactCleanupExecutor(cfg.Logger, cfg.DataDir),
		reconcileTicks: func(interval time.Duration) (<-chan time.Time, func()) {
			if interval <= 0 {
				return nil, func() {}
			}

			ticker := time.NewTicker(interval)
			return ticker.C, ticker.Stop
		},
	}
}
