package multisync

import (
	"context"
	"fmt"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func (o *Orchestrator) buildRuntimeWorkSet(
	ctx context.Context,
	standaloneMounts []StandaloneMountConfig,
	initialStartup []MountStartupResult,
) (*runtimeWorkSet, error) {
	if ctx == nil {
		return nil, fmt.Errorf("building runtime work: context is required")
	}

	parents, err := buildStandaloneMountSpecs(standaloneMounts)
	if err != nil {
		return nil, err
	}

	parentSnapshots := o.latestParentChildWorkSnapshotsFor(parents)
	decisions, err := buildRuntimeWorkForParents(parents, parentSnapshots, o.cfg.DataDir, o.logger)
	if err != nil {
		return nil, err
	}
	decisions.CleanupScopeAllParents = true
	offsetRuntimeWorkSelectionIndexes(decisions, nextStartupSelectionIndex(initialStartup))
	decisions.Skipped = append(append([]MountStartupResult(nil), initialStartup...), decisions.Skipped...)

	return decisions, nil
}

func (o *Orchestrator) buildRuntimeWorkFromParentSnapshots(
	standaloneMounts []StandaloneMountConfig,
	initialStartup []MountStartupResult,
) (*runtimeWorkSet, error) {
	parents, err := buildStandaloneMountSpecs(standaloneMounts)
	if err != nil {
		return nil, err
	}
	parentSnapshots := o.latestParentChildWorkSnapshotsFor(parents)
	decisions, err := buildRuntimeWorkForParents(parents, parentSnapshots, o.cfg.DataDir, o.logger)
	if err != nil {
		return nil, err
	}
	decisions.CleanupScopeAllParents = true
	offsetRuntimeWorkSelectionIndexes(decisions, nextStartupSelectionIndex(initialStartup))
	decisions.Skipped = append(append([]MountStartupResult(nil), initialStartup...), decisions.Skipped...)

	return decisions, nil
}

func (o *Orchestrator) buildRuntimeWorkForParent(parent *mountSpec) (*runtimeWorkSet, error) {
	if parent == nil {
		return nil, fmt.Errorf("building parent child runtime work: parent mount is required")
	}
	parentCopy := cloneMountSpec(parent)
	parentSnapshots := map[mountID]syncengine.ShortcutChildWorkSnapshot{
		parentCopy.id(): o.latestParentChildWorkSnapshotFor(parentCopy.id()),
	}
	return buildRuntimeWorkForParents([]*mountSpec{parentCopy}, parentSnapshots, o.cfg.DataDir, o.logger)
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

func offsetRuntimeWorkSelectionIndexes(decisions *runtimeWorkSet, offset int) {
	if decisions == nil || offset == 0 {
		return
	}
	for i := range decisions.Mounts {
		decisions.Mounts[i].setSelectionIndex(decisions.Mounts[i].selectionIndex() + offset)
	}
	for i := range decisions.Skipped {
		decisions.Skipped[i].SelectionIndex += offset
	}
}

func cloneMountSpec(mount *mountSpec) *mountSpec {
	if mount == nil {
		return nil
	}
	cloned := *mount
	if mount.parent != nil {
		parent := *mount.parent
		cloned.parent = &parent
	}
	if mount.child != nil {
		child := *mount.child
		child.engine = cloneShortcutChildEngineSpec(mount.child.engine)
		cloned.child = &child
	}
	return &cloned
}
