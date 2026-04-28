package multisync

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	gosync "sync"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// oneShotChildRuns owns child runner timing for a single RunOnce call. It is
// not shortcut policy: parents publish exact child work snapshots and children
// execute ordinary Engine.RunOnce work. State machine:
// parent registered -> exact snapshot accepted -> parent safe point reached ->
// child work started -> final-drain/artifact cleanup acked. If the context is
// canceled before the safe point, no child work starts; parent shortcut_roots
// plus child artifacts remain the recovery source.
type oneShotChildRuns struct {
	orchestrator *Orchestrator
	mode         syncengine.SyncMode
	opts         syncengine.RunOptions

	mu            gosync.Mutex
	cache         *parentChildWorkSnapshots
	parents       map[mountID]*mountSpec
	parentAckers  map[mountID]shortcutChildAckHandle
	parentDone    map[mountID]bool
	snapshots     map[mountID]syncengine.ShortcutChildWorkSnapshot
	started       map[mountID]bool
	startup       []MountStartupResult
	childReports  []*MountReport
	childRunGroup gosync.WaitGroup
}

func newOneShotChildRuns(
	orchestrator *Orchestrator,
	mode syncengine.SyncMode,
	opts syncengine.RunOptions,
	parents []*mountSpec,
) *oneShotChildRuns {
	parentByID := make(map[mountID]*mountSpec, len(parents))
	for _, parent := range parents {
		if parent == nil {
			continue
		}
		parentByID[parent.id()] = cloneMountSpec(parent)
	}
	return &oneShotChildRuns{
		orchestrator: orchestrator,
		mode:         mode,
		opts:         opts,
		cache:        newParentChildWorkSnapshots(),
		parents:      parentByID,
		parentAckers: make(map[mountID]shortcutChildAckHandle),
		parentDone:   make(map[mountID]bool),
		snapshots:    make(map[mountID]syncengine.ShortcutChildWorkSnapshot),
		started:      make(map[mountID]bool),
	}
}

func (c *oneShotChildRuns) registerParents(work []indexedMountWork) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, item := range work {
		if item.work.mount == nil || item.work.mount.projectionKind() != MountProjectionStandalone {
			continue
		}
		parentID := item.work.mount.id()
		if !shortcutChildAckHandleIsZero(item.work.parentAck) {
			c.parentAckers[parentID] = item.work.parentAck
		}
	}
}

func (c *oneShotChildRuns) notifyParentSnapshot(
	ctx context.Context,
	parentID mountID,
	snapshot syncengine.ShortcutChildWorkSnapshot,
) error {
	if c == nil || parentID == "" {
		return nil
	}

	c.mu.Lock()
	c.snapshots[parentID] = snapshot
	c.mu.Unlock()
	c.startChildrenForParent(ctx, parentID)
	return nil
}

func (c *oneShotChildRuns) markParentDone(ctx context.Context, parentID mountID) {
	if c == nil || parentID == "" {
		return
	}
	c.mu.Lock()
	c.parentDone[parentID] = true
	c.mu.Unlock()
	c.startChildrenForParent(ctx, parentID)
}

func (c *oneShotChildRuns) startChildrenForParent(ctx context.Context, parentID mountID) {
	if c == nil || parentID == "" {
		return
	}
	if ctx != nil && ctx.Err() != nil {
		return
	}
	c.mu.Lock()
	if c.started[parentID] || !c.parentDone[parentID] {
		c.mu.Unlock()
		return
	}
	parent := cloneMountSpec(c.parents[parentID])
	if parent == nil {
		c.mu.Unlock()
		return
	}
	snapshot := c.snapshots[parentID]
	if !shortcutChildWorkSnapshotHasWork(snapshot) {
		c.mu.Unlock()
		return
	}
	c.started[parentID] = true
	c.childRunGroup.Add(1)
	c.mu.Unlock()

	go c.runChildrenForParent(ctx, parentID, parent, snapshot)
}

func (c *oneShotChildRuns) wait() {
	if c == nil {
		return
	}
	c.childRunGroup.Wait()
}

func (c *oneShotChildRuns) startupResults() []MountStartupResult {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	results := append([]MountStartupResult(nil), c.startup...)
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].SelectionIndex < results[j].SelectionIndex
	})
	return results
}

func (c *oneShotChildRuns) reports() []*MountReport {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	reports := append([]*MountReport(nil), c.childReports...)
	sort.SliceStable(reports, func(i, j int) bool {
		if reports[i] == nil || reports[j] == nil {
			return reports[j] != nil
		}
		return reports[i].SelectionIndex < reports[j].SelectionIndex
	})
	return reports
}

func (c *oneShotChildRuns) runChildrenForParent(
	ctx context.Context,
	parentID mountID,
	parent *mountSpec,
	snapshot syncengine.ShortcutChildWorkSnapshot,
) {
	defer c.childRunGroup.Done()
	decisions, err := c.orchestrator.buildRuntimeWorkForParentSnapshot(parent, snapshot)
	if err != nil {
		c.appendStartup(MountStartupResult{
			SelectionIndex: parent.selectionIndex(),
			Identity:       parent.identity(),
			DisplayName:    parent.displayName(),
			Status:         MountStartupFatal,
			Err:            fmt.Errorf("building child mount specs after parent snapshot: %w", err),
		})
		return
	}

	childMounts := filterMountSpecsByProjection(decisions.Mounts, MountProjectionChild)
	childWork, childStartup, childReports := c.orchestrator.prepareRunOnceWork(
		ctx,
		c.mode,
		childMounts,
		decisions.Skipped,
		c.opts,
		nil,
		nil,
	)
	c.appendStartup(childStartup.Results...)
	runIndexedMountWork(ctx, childWork, childReports)
	c.orchestrator.closeRunOnceChildEngines(ctx, childWork)
	c.appendReports(childReports...)

	parentAckers := c.parentAckersFor(parentID)
	if purgeErr := c.orchestrator.purgeShortcutChildArtifactsForDecisions(
		ctx,
		decisions,
		cloneParentAckHandles(parentAckers),
		c.cache,
	); purgeErr != nil {
		c.orchestrator.logger.Warn("purging shortcut child state artifacts",
			slog.String("parent_mount_id", parentID.String()),
			slog.String("error", purgeErr.Error()),
		)
		c.appendReports(&MountReport{
			SelectionIndex: parent.selectionIndex(),
			Identity:       parent.identity(),
			DisplayName:    parent.displayName(),
			Err:            fmt.Errorf("shortcut child artifact cleanup: %w", purgeErr),
		})
	}
	if finalized, finalizeErr := c.orchestrator.finalizeSuccessfulFinalDrainMounts(
		ctx,
		decisions,
		childReports,
		parentAckers,
		c.cache,
	); finalizeErr != nil {
		c.orchestrator.logger.Warn("finalizing drained shortcut child mounts",
			slog.String("parent_mount_id", parentID.String()),
			slog.String("error", finalizeErr.Error()),
		)
		c.appendReports(&MountReport{
			SelectionIndex: parent.selectionIndex(),
			Identity:       parent.identity(),
			DisplayName:    parent.displayName(),
			Err:            fmt.Errorf("shortcut child final-drain cleanup: %w", finalizeErr),
		})
	} else if finalized {
		c.orchestrator.logger.Info("finalized drained shortcut child mounts",
			slog.String("parent_mount_id", parentID.String()),
		)
	}
}

func (c *oneShotChildRuns) parentAckersFor(parentID mountID) map[mountID]shortcutChildAckHandle {
	c.mu.Lock()
	defer c.mu.Unlock()
	acker := c.parentAckers[parentID]
	if shortcutChildAckHandleIsZero(acker) {
		return nil
	}
	return map[mountID]shortcutChildAckHandle{parentID: acker}
}

func (c *oneShotChildRuns) appendStartup(results ...MountStartupResult) {
	if c == nil || len(results) == 0 {
		return
	}
	c.mu.Lock()
	c.startup = append(c.startup, results...)
	c.mu.Unlock()
}

func (c *oneShotChildRuns) appendReports(reports ...*MountReport) {
	if c == nil || len(reports) == 0 {
		return
	}
	c.mu.Lock()
	c.childReports = append(c.childReports, reports...)
	c.mu.Unlock()
}
