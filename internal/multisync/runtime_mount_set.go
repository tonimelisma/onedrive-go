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
	if ctx == nil {
		return nil, fmt.Errorf("building runtime mount set: context is required")
	}

	parents, err := buildStandaloneMountSpecs(p.standaloneMounts)
	if err != nil {
		return nil, err
	}

	inventory, loadErr := config.LoadMountInventory()
	if loadErr != nil {
		return nil, fmt.Errorf("loading mount inventory: %w", loadErr)
	}

	dirtyMountIDs := make([]string, 0)
	var persistErr error
	deferredResult := promoteDeferredShortcutBindings(inventory, parents)
	dirtyMountIDs = appendUniqueStrings(dirtyMountIDs, deferredResult.dirtyMountIDs...)
	if deferredResult.changed {
		if saveErr := config.SaveMountInventory(inventory); saveErr != nil {
			p.orchestrator.warnChildRootReconciliationSaveFailure(saveErr)
			persistErr = errors.Join(persistErr, fmt.Errorf("saving mount inventory after deferred shortcut promotion: %w", saveErr))
		}
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
	compiled.LocalRootActions = append(compiled.LocalRootActions, localRootResult.localRootActions...)
	attachLocalRootActionPresentation(compiled)
	compiled.RemovedMountIDs = append(compiled.RemovedMountIDs, pendingRemovalMountIDs(inventory)...)
	applyInventoryPersistFailure(compiled, dirtyMountIDs, persistErr)
	offsetCompiledSelectionIndexes(compiled, nextStartupSelectionIndex(p.initialStartup))
	compiled.Skipped = append(append([]MountStartupResult(nil), p.initialStartup...), compiled.Skipped...)

	return compiled, nil
}

func attachLocalRootActionPresentation(compiled *compiledMountSet) {
	if compiled == nil || len(compiled.LocalRootActions) == 0 {
		return
	}
	mounts := make(map[mountID]*mountSpec, len(compiled.Mounts))
	for _, mount := range compiled.Mounts {
		if mount != nil {
			mounts[mount.mountID] = mount
		}
	}
	for i := range compiled.LocalRootActions {
		if mount := mounts[compiled.LocalRootActions[i].mountID]; mount != nil {
			compiled.LocalRootActions[i].selectionIndex = mount.selectionIndex
			compiled.LocalRootActions[i].identity = mount.identity()
			compiled.LocalRootActions[i].displayName = mount.displayName
		}
	}
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
	compiled.RemovedMountIDs = append(compiled.RemovedMountIDs, pendingRemovalMountIDs(inventory)...)
	offsetCompiledSelectionIndexes(compiled, nextStartupSelectionIndex(initialStartup))
	compiled.Skipped = append(append([]MountStartupResult(nil), initialStartup...), compiled.Skipped...)

	return compiled, nil
}

func (o *Orchestrator) finalizeRuntimeMountSetLifecycle(
	ctx context.Context,
	compiled *compiledMountSet,
	standaloneMounts []StandaloneMountConfig,
	initialStartup []MountStartupResult,
	phase string,
	strictRecompile bool,
) (*compiledMountSet, error) {
	rootActionsChanged := applyChildRootLifecycleActions(ctx, o, compiled, o.logger)
	if rootActionsChanged {
		refreshed, recompiled, refreshErr := o.refreshRuntimeMountSetAfterLifecycleMutation(
			compiled,
			standaloneMounts,
			initialStartup,
			phase,
			strictRecompile,
		)
		if refreshErr != nil {
			return compiled, refreshErr
		}
		compiled = refreshed
		if !recompiled {
			compiled.ProjectionMoves = nil
		}
	}

	inventoryMutated := false
	finalized, finalizeErr := finalizePendingMountRemovals(compiled.RemovedMountIDs, compiled.Mounts, o.logger)
	if finalizeErr != nil {
		o.logger.Warn("finalizing removed child mounts",
			slog.String("phase", phase),
			slog.String("error", finalizeErr.Error()),
		)
	}
	inventoryMutated = inventoryMutated || finalized
	inventoryMutated = applyChildProjectionMoves(compiled, o.logger) || inventoryMutated
	if inventoryMutated {
		refreshed, _, refreshErr := o.refreshRuntimeMountSetAfterLifecycleMutation(
			compiled,
			standaloneMounts,
			initialStartup,
			phase,
			strictRecompile,
		)
		if refreshErr != nil {
			return compiled, refreshErr
		}
		compiled = refreshed
	}
	validateCompiledChildMountRoots(compiled, o.logger)

	return compiled, nil
}

func (o *Orchestrator) refreshRuntimeMountSetAfterLifecycleMutation(
	current *compiledMountSet,
	standaloneMounts []StandaloneMountConfig,
	initialStartup []MountStartupResult,
	phase string,
	strictRecompile bool,
) (*compiledMountSet, bool, error) {
	refreshed, refreshErr := o.compileRuntimeMountSetFromInventory(standaloneMounts, initialStartup)
	if refreshErr != nil {
		if strictRecompile {
			return current, false, fmt.Errorf("rebuilding mount specs after lifecycle mutation: %w", refreshErr)
		}
		o.logger.Warn("rebuilding mount specs after lifecycle mutation failed; using current mount set",
			slog.String("phase", phase),
			slog.String("error", refreshErr.Error()),
		)
		return current, false, nil
	}

	return refreshed, true, nil
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

	filteredRootActions := compiled.LocalRootActions[:0]
	for i := range compiled.LocalRootActions {
		action := compiled.LocalRootActions[i]
		if _, found := dirty[action.mountID]; found {
			continue
		}
		filteredRootActions = append(filteredRootActions, action)
	}
	compiled.LocalRootActions = filteredRootActions
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
