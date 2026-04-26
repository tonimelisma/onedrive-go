package multisync

import (
	"context"
	"fmt"
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

	parentTopologies := p.orchestrator.parentShortcutTopologiesFor(parents)
	compiled, err := compileRuntimeMountsForParents(parents, parentTopologies, p.orchestrator.logger)
	if err != nil {
		return nil, err
	}
	offsetCompiledSelectionIndexes(compiled, nextStartupSelectionIndex(p.initialStartup))
	compiled.Skipped = append(append([]MountStartupResult(nil), p.initialStartup...), compiled.Skipped...)

	return compiled, nil
}

func (o *Orchestrator) compileRuntimeMountSetFromTopology(
	standaloneMounts []StandaloneMountConfig,
	initialStartup []MountStartupResult,
) (*compiledMountSet, error) {
	parents, err := buildStandaloneMountSpecs(standaloneMounts)
	if err != nil {
		return nil, err
	}
	parentTopologies := o.parentShortcutTopologiesFor(parents)
	compiled, err := compileRuntimeMountsForParents(parents, parentTopologies, o.logger)
	if err != nil {
		return nil, err
	}
	offsetCompiledSelectionIndexes(compiled, nextStartupSelectionIndex(initialStartup))
	compiled.Skipped = append(append([]MountStartupResult(nil), initialStartup...), compiled.Skipped...)

	return compiled, nil
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
