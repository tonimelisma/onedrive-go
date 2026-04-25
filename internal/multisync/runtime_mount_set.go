package multisync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

type runtimeMountSetPipeline struct {
	orchestrator     *Orchestrator
	standaloneMounts []StandaloneMountConfig
	initialStartup   []MountStartupResult
}

func (o *Orchestrator) buildRuntimeMountSet(
	ctx context.Context,
	standaloneMounts []StandaloneMountConfig,
	initialStartup []MountStartupResult,
) (*compiledMountSet, error) {
	pipeline := runtimeMountSetPipeline{
		orchestrator:     o,
		standaloneMounts: standaloneMounts,
		initialStartup:   initialStartup,
	}
	return pipeline.build(ctx)
}

func (p runtimeMountSetPipeline) build(ctx context.Context) (*compiledMountSet, error) {
	parents, err := buildStandaloneMountSpecs(p.standaloneMounts)
	if err != nil {
		return nil, err
	}

	namespaceResult, reconcileErr := p.orchestrator.reconcileManagedShortcutMounts(ctx, parents)
	if reconcileErr != nil && p.orchestrator.logger != nil {
		p.orchestrator.logger.Warn("shortcut reconciliation failed; keeping existing mount inventory",
			slog.String("error", reconcileErr.Error()),
		)
	}

	inventory := namespaceResult.inventory
	if inventory == nil {
		loadedInventory, loadErr := config.LoadMountInventory()
		if loadErr != nil {
			return nil, fmt.Errorf("loading mount inventory: %w", loadErr)
		}
		inventory = loadedInventory
	}

	dirtyMountIDs := appendUniqueStrings(nil, namespaceResult.dirtyMountIDs...)
	var persistErr error
	if namespaceResult.persistErr != nil {
		persistErr = namespaceResult.persistErr
	}
	localRootResult := reconcileChildMountLocalRoots(parents, inventory, p.orchestrator.logger)
	dirtyMountIDs = appendUniqueStrings(dirtyMountIDs, localRootResult.dirtyMountIDs...)
	if localRootResult.changed {
		if saveErr := config.SaveMountInventory(inventory); saveErr != nil {
			p.orchestrator.warnChildRootReconciliationSaveFailure(saveErr)
			persistErr = errors.Join(persistErr, fmt.Errorf("saving mount inventory after child root reconciliation: %w", saveErr))
		}
	}

	compiled, err := compileRuntimeMountsForParents(parents, inventory, p.orchestrator.logger)
	if err != nil {
		return nil, err
	}
	if reconcileErr == nil && namespaceResult.persistErr == nil {
		compiled.RemovedMountIDs = append(compiled.RemovedMountIDs, namespaceResult.removedMountIDs...)
	}
	applyInventoryPersistFailure(compiled, dirtyMountIDs, persistErr)
	offsetCompiledSelectionIndexes(compiled, nextStartupSelectionIndex(p.initialStartup))
	compiled.Skipped = append(append([]MountStartupResult(nil), p.initialStartup...), compiled.Skipped...)

	return compiled, nil
}

func (o *Orchestrator) compileRuntimeMountSetFromInventory(
	standaloneMounts []StandaloneMountConfig,
	initialStartup []MountStartupResult,
) (*compiledMountSet, error) {
	parents, err := buildStandaloneMountSpecs(standaloneMounts)
	if err != nil {
		return nil, err
	}
	inventory, err := config.LoadMountInventory()
	if err != nil {
		return nil, fmt.Errorf("loading mount inventory: %w", err)
	}
	compiled, err := compileRuntimeMountsForParents(parents, inventory, o.logger)
	if err != nil {
		return nil, err
	}
	offsetCompiledSelectionIndexes(compiled, nextStartupSelectionIndex(initialStartup))
	compiled.Skipped = append(append([]MountStartupResult(nil), initialStartup...), compiled.Skipped...)

	return compiled, nil
}

func (o *Orchestrator) finalizeRuntimeMountSetLifecycle(
	compiled *compiledMountSet,
	standaloneMounts []StandaloneMountConfig,
	initialStartup []MountStartupResult,
	phase string,
	strictRecompile bool,
) (*compiledMountSet, error) {
	inventoryMutated := false
	if purgeErr := purgeManagedMountStateDBs(o.logger, compiled.RemovedMountIDs); purgeErr != nil {
		o.logger.Warn("purging removed child mount state",
			slog.String("phase", phase),
			slog.String("error", purgeErr.Error()),
		)
	} else {
		finalized, finalizeErr := finalizePendingMountRemovals(compiled.RemovedMountIDs)
		if finalizeErr != nil {
			o.logger.Warn("finalizing removed child mounts",
				slog.String("phase", phase),
				slog.String("error", finalizeErr.Error()),
			)
		}
		inventoryMutated = inventoryMutated || finalized
	}
	inventoryMutated = applyChildProjectionMoves(compiled, o.logger) || inventoryMutated
	if inventoryMutated {
		refreshed, refreshErr := o.compileRuntimeMountSetFromInventory(standaloneMounts, initialStartup)
		if refreshErr != nil {
			if strictRecompile {
				return compiled, fmt.Errorf("rebuilding mount specs after lifecycle mutation: %w", refreshErr)
			}
			o.logger.Warn("rebuilding mount specs after lifecycle mutation failed; using current mount set",
				slog.String("phase", phase),
				slog.String("error", refreshErr.Error()),
			)
		} else {
			compiled = refreshed
		}
	}
	validateCompiledChildMountRoots(compiled, o.logger)

	return compiled, nil
}

func applyInventoryPersistFailure(compiled *compiledMountSet, dirtyMountIDs []string, err error) {
	if compiled == nil || err == nil || len(dirtyMountIDs) == 0 {
		return
	}

	dirty := make(map[mountID]struct{}, len(dirtyMountIDs))
	for _, id := range dirtyMountIDs {
		if id != "" {
			dirty[mountID(id)] = struct{}{}
		}
	}

	filtered := compiled.Mounts[:0]
	for _, mount := range compiled.Mounts {
		if mount.projectionKind == MountProjectionChild {
			if _, found := dirty[mount.mountID]; found {
				compiled.Skipped = append(compiled.Skipped, mountStartupResultForMount(
					mount,
					fmt.Errorf("child mount %s has unpersisted lifecycle state: %w", mount.mountID, err),
				))
				continue
			}
		}
		filtered = append(filtered, mount)
	}
	compiled.Mounts = filtered

	filteredMoves := compiled.ProjectionMoves[:0]
	for i := range compiled.ProjectionMoves {
		move := compiled.ProjectionMoves[i]
		if _, found := dirty[move.mountID]; found {
			continue
		}
		filteredMoves = append(filteredMoves, move)
	}
	compiled.ProjectionMoves = filteredMoves
}

func (o *Orchestrator) warnChildRootReconciliationSaveFailure(err error) {
	if err == nil || o.logger == nil {
		return
	}

	o.logger.Warn("child mount local root reconciliation was not persisted; continuing with in-memory mount inventory",
		slog.String("error", err.Error()),
	)
}

func nextStartupSelectionIndex(results []MountStartupResult) int {
	next := 0
	for i := range results {
		if results[i].SelectionIndex >= next {
			next = results[i].SelectionIndex + 1
		}
	}

	return next
}

func offsetCompiledSelectionIndexes(compiled *compiledMountSet, offset int) {
	if compiled == nil || offset == 0 {
		return
	}
	for i := range compiled.Mounts {
		compiled.Mounts[i].selectionIndex += offset
	}
	for i := range compiled.Skipped {
		compiled.Skipped[i].SelectionIndex += offset
	}
}
