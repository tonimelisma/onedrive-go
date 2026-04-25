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
)

// engineRunner is the interface the Orchestrator uses to run sync passes.
// Implemented by *sync.Engine; mock implementations are used in tests.
type engineRunner interface {
	RunOnce(ctx context.Context, mode syncengine.SyncMode, opts syncengine.RunOptions) (*syncengine.Report, error)
	RunWatch(ctx context.Context, mode syncengine.SyncMode, opts syncengine.WatchOptions) error
	Close(ctx context.Context) error
}

type engineFactoryRequest struct {
	Session           *driveops.Session
	Mount             *mountSpec
	Logger            *slog.Logger
	VerifyDrive       bool
	PerfCollector     *perf.Collector
	ManagedRootEvents syncengine.ManagedRootEventSink
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
	Logger                 *slog.Logger
	ControlSocketPath      string
	StartWarning           func(StartupWarning)
	DebugEventHook         func(syncengine.DebugEvent)
	PerfParent             *perf.Collector
}

// Orchestrator manages per-mount sync runners. It is always used, even for a
// single mount, so one-shot and watch mode share the same top-level lifecycle.
type Orchestrator struct {
	cfg            *OrchestratorConfig
	engineFactory  engineFactoryFunc // injectable for tests
	logger         *slog.Logger
	perfRuntime    *perf.Runtime
	statusMu       gosync.RWMutex
	controlMounts  []string
	reconcileTicks func(time.Duration) (<-chan time.Time, func())
}

// NewOrchestrator creates an Orchestrator with real Engine factory.
// Token/client caching is handled by the SessionRuntime in cfg.Runtime.
// Tests inject stubs via cfg.Runtime.TokenSourceFn and engineFactory.
func NewOrchestrator(cfg *OrchestratorConfig) *Orchestrator {
	return &Orchestrator{
		cfg: cfg,
		engineFactory: func(ctx context.Context, req engineFactoryRequest) (engineRunner, error) {
			mountCfg, err := engineMountConfigForMount(req.Mount)
			if err != nil {
				return nil, fmt.Errorf("engine mount config: %w", err)
			}
			mountCfg.ManagedRootEvents = req.ManagedRootEvents
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
			return engine, nil
		},
		logger:      cfg.Logger,
		perfRuntime: perf.NewRuntime(cfg.PerfParent),
		reconcileTicks: func(interval time.Duration) (<-chan time.Time, func()) {
			if interval <= 0 {
				return nil, func() {}
			}

			ticker := time.NewTicker(interval)
			return ticker.C, ticker.Stop
		},
	}
}
