package multisync

import (
	"context"
	"fmt"
	"log/slog"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type parentTopologyPreparer interface {
	PrepareInitialTopology(
		context.Context,
		syncengine.SyncMode,
		syncengine.RunOptions,
	) (syncengine.ShortcutChildTopologySnapshot, error)
}

type preparedParentEngines map[mountID]engineRunner

func (o *Orchestrator) prepareParentTopology(
	ctx context.Context,
	compiled *compiledMountSet,
	standaloneMounts []StandaloneMountConfig,
	initialStartup []MountStartupResult,
	existingWatchRunners map[mountID]*watchRunner,
	mode syncengine.SyncMode,
	opts syncengine.RunOptions,
) (*compiledMountSet, preparedParentEngines, error) {
	prepared := make(preparedParentEngines)
	if compiled == nil || o == nil || o.cfg == nil || o.cfg.disableParentTopologyPrepare {
		return compiled, prepared, nil
	}

	changed := false
	for i := range compiled.Mounts {
		mount := compiled.Mounts[i]
		if mount == nil || mount.paused || mount.projectionKind != MountProjectionStandalone {
			continue
		}
		if canReuseWatchRunnerForParentPrepare(existingWatchRunners, mount) {
			continue
		}

		parentChanged, engine, err := o.prepareTopologyForParent(ctx, mount, mode, opts)
		if err != nil {
			o.closePreparedParentEngines(ctx, prepared)
			return compiled, nil, fmt.Errorf("prepare parent topology for mount %s: %w", mount.label(), err)
		}
		if engine != nil {
			prepared[mount.mountID] = engine
		}
		changed = changed || parentChanged
	}
	if !changed {
		return compiled, prepared, nil
	}

	refreshed, err := o.buildRuntimeMountSet(ctx, standaloneMounts, initialStartup)
	if err != nil {
		o.closePreparedParentEngines(ctx, prepared)
		return compiled, nil, fmt.Errorf("rebuilding mount specs after parent topology prepare: %w", err)
	}
	return refreshed, prepared, nil
}

func canReuseWatchRunnerForParentPrepare(
	existingWatchRunners map[mountID]*watchRunner,
	parent *mountSpec,
) bool {
	if parent == nil || len(existingWatchRunners) == 0 {
		return false
	}
	runner := existingWatchRunners[parent.mountID]
	if runner == nil || runner.mount == nil {
		return false
	}
	return mountSpecCoreEquivalent(runner.mount, parent)
}

func (o *Orchestrator) prepareTopologyForParent(
	ctx context.Context,
	parent *mountSpec,
	mode syncengine.SyncMode,
	opts syncengine.RunOptions,
) (bool, engineRunner, error) {
	if o == nil || o.cfg == nil || parent == nil {
		return false, nil, nil
	}

	session, err := o.cfg.Runtime.SyncSession(ctx, parent.syncSessionConfig())
	if err != nil {
		return false, nil, fmt.Errorf("session error for mount %s: %w", parent.label(), err)
	}

	preparedParent := *parent
	mountCollector := o.registerMountPerfCollector(parent.mountID.String())
	engine, err := o.engineFactory(ctx, engineFactoryRequest{
		Session:       session,
		Mount:         &preparedParent,
		Logger:        o.logger,
		VerifyDrive:   true,
		PerfCollector: mountCollector,
	})
	if err != nil {
		o.removeMountPerfCollector(parent.mountID.String())
		return false, nil, fmt.Errorf("engine creation failed for mount %s: %w", parent.label(), err)
	}

	preparer, ok := engine.(parentTopologyPreparer)
	if !ok {
		return false, engine, nil
	}
	snapshot, err := preparer.PrepareInitialTopology(ctx, mode, opts)
	if err != nil {
		o.closePreparedParentEngine(ctx, parent.mountID, engine)
		return false, nil, fmt.Errorf("preparing parent initial topology: %w", err)
	}
	changed := o.storeParentShortcutTopology(parent.mountID, snapshot)

	return changed, engine, nil
}

func (o *Orchestrator) closePreparedParentEngines(
	ctx context.Context,
	prepared preparedParentEngines,
) {
	for id, engine := range prepared {
		o.closePreparedParentEngine(ctx, id, engine)
		delete(prepared, id)
	}
}

func (o *Orchestrator) closePreparedParentEngine(
	ctx context.Context,
	id mountID,
	engine engineRunner,
) {
	if engine == nil {
		return
	}
	defer o.removeMountPerfCollector(id.String())
	if closeErr := engine.Close(ctx); closeErr != nil && o.logger != nil {
		o.logger.Warn("engine close error after parent topology prepare",
			slog.String("mount_id", id.String()),
			slog.String("error", closeErr.Error()),
		)
	}
}
