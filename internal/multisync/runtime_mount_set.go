package multisync

import (
	"context"
	"fmt"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func (o *Orchestrator) buildRunnerDecisionSet(
	ctx context.Context,
	standaloneMounts []StandaloneMountConfig,
	initialStartup []MountStartupResult,
) (*runnerDecisionSet, error) {
	if ctx == nil {
		return nil, fmt.Errorf("building runner decisions: context is required")
	}

	parents, err := buildStandaloneMountSpecs(standaloneMounts)
	if err != nil {
		return nil, err
	}

	parentSnapshots := o.latestParentChildWorkSnapshotsFor(parents)
	decisions, err := buildRunnerDecisionsForParents(parents, parentSnapshots, o.cfg.DataDir, o.logger)
	if err != nil {
		return nil, err
	}
	offsetRunnerDecisionSelectionIndexes(decisions, nextStartupSelectionIndex(initialStartup))
	decisions.Skipped = append(append([]MountStartupResult(nil), initialStartup...), decisions.Skipped...)

	return decisions, nil
}

func (o *Orchestrator) buildRunnerDecisionsFromParentSnapshots(
	standaloneMounts []StandaloneMountConfig,
	initialStartup []MountStartupResult,
) (*runnerDecisionSet, error) {
	parents, err := buildStandaloneMountSpecs(standaloneMounts)
	if err != nil {
		return nil, err
	}
	parentSnapshots := o.latestParentChildWorkSnapshotsFor(parents)
	decisions, err := buildRunnerDecisionsForParents(parents, parentSnapshots, o.cfg.DataDir, o.logger)
	if err != nil {
		return nil, err
	}
	offsetRunnerDecisionSelectionIndexes(decisions, nextStartupSelectionIndex(initialStartup))
	decisions.Skipped = append(append([]MountStartupResult(nil), initialStartup...), decisions.Skipped...)

	return decisions, nil
}

func (o *Orchestrator) buildRunnerDecisionsForParent(parent *mountSpec) (*runnerDecisionSet, error) {
	if parent == nil {
		return nil, fmt.Errorf("building parent child runner decisions: parent mount is required")
	}
	parentCopy := cloneMountSpec(parent)
	parentSnapshots := map[mountID]syncengine.ShortcutChildWorkSnapshot{
		parentCopy.mountID: o.latestParentChildWorkSnapshotFor(parentCopy.mountID),
	}
	return buildRunnerDecisionsForParents([]*mountSpec{parentCopy}, parentSnapshots, o.cfg.DataDir, o.logger)
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

func offsetRunnerDecisionSelectionIndexes(decisions *runnerDecisionSet, offset int) {
	if decisions == nil || offset == 0 {
		return
	}
	for i := range decisions.Mounts {
		decisions.Mounts[i].selectionIndex += offset
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
	if mount.child != nil {
		child := *mount.child
		child.engine = cloneShortcutChildEngineSpec(mount.child.engine)
		cloned.child = &child
	}
	return &cloned
}
