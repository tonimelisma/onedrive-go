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
// not shortcut policy: parents publish exact runner snapshots, children execute
// ordinary Engine.RunOnce work, and this coordinator only preserves the
// ordering rule that final-drain and artifact-cleanup acknowledgements go back
// to the same live parent after that parent reaches its safe ack point.
type oneShotChildRuns struct {
	orchestrator *Orchestrator
	mode         syncengine.SyncMode
	opts         syncengine.RunOptions

	mu            gosync.Mutex
	parents       map[mountID]*mountSpec
	parentDone    map[mountID]chan struct{}
	parentAckers  map[mountID]shortcutChildAckHandle
	published     map[mountID]bool
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
		parentByID[parent.mountID] = cloneMountSpec(parent)
	}
	return &oneShotChildRuns{
		orchestrator: orchestrator,
		mode:         mode,
		opts:         opts,
		parents:      parentByID,
		parentDone:   make(map[mountID]chan struct{}),
		parentAckers: make(map[mountID]shortcutChildAckHandle),
		published:    make(map[mountID]bool),
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
		if item.work.mount == nil || item.work.mount.projectionKind != MountProjectionStandalone {
			continue
		}
		parentID := item.work.mount.mountID
		if _, found := c.parentDone[parentID]; !found {
			c.parentDone[parentID] = make(chan struct{})
		}
		// One-shot children must come from the live parent run, not from
		// an old publication cached before this parent started.
		c.orchestrator.forgetParentRunnerPublication(parentID)
		if !shortcutChildAckHandleIsZero(item.work.parentAck) {
			c.parentAckers[parentID] = item.work.parentAck
		}
	}
}

func (c *oneShotChildRuns) notifyParentPublication(ctx context.Context, parentID mountID) error {
	if c == nil || parentID == "" {
		return nil
	}

	c.mu.Lock()
	c.published[parentID] = true
	c.mu.Unlock()
	c.startChildrenForParent(ctx, parentID)
	return nil
}

func (c *oneShotChildRuns) markParentDone(parentID mountID) {
	if c == nil || parentID == "" {
		return
	}
	c.mu.Lock()
	done := c.parentDone[parentID]
	if done != nil {
		delete(c.parentDone, parentID)
	}
	c.mu.Unlock()
	if done != nil {
		close(done)
	}
}

func (c *oneShotChildRuns) startChildrenForParent(ctx context.Context, parentID mountID) {
	if c == nil || parentID == "" {
		return
	}
	c.mu.Lock()
	if c.started[parentID] || !c.published[parentID] {
		c.mu.Unlock()
		return
	}
	parent := cloneMountSpec(c.parents[parentID])
	if parent == nil {
		c.mu.Unlock()
		return
	}
	if !c.orchestrator.parentRunnerPublicationHasOneShotWork(parentID) {
		c.mu.Unlock()
		return
	}
	c.started[parentID] = true
	c.childRunGroup.Add(1)
	c.mu.Unlock()

	go c.runChildrenForParent(ctx, parentID, parent)
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
) {
	defer c.childRunGroup.Done()
	decisions, err := c.orchestrator.buildRunnerDecisionsForParent(parent)
	if err != nil {
		c.appendStartup(MountStartupResult{
			SelectionIndex: parent.selectionIndex,
			Identity:       parent.identity(),
			DisplayName:    parent.displayName,
			Status:         MountStartupFatal,
			Err:            fmt.Errorf("building child mount specs after parent publication: %w", err),
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
	)
	c.appendStartup(childStartup.Results...)
	runIndexedMountWork(ctx, childWork, childReports)
	c.orchestrator.closeRunOnceChildEngines(ctx, childWork)
	c.appendReports(childReports...)

	c.waitParentDone(parentID)
	parentAckers := c.parentAckersFor(parentID)
	if purgeErr := c.orchestrator.purgeShortcutChildArtifactsForDecisions(
		ctx,
		decisions,
		cloneParentAckHandles(parentAckers),
	); purgeErr != nil {
		c.orchestrator.logger.Warn("purging shortcut child state artifacts",
			slog.String("parent_mount_id", parentID.String()),
			slog.String("error", purgeErr.Error()),
		)
	}
	if finalized, finalizeErr := c.orchestrator.finalizeSuccessfulFinalDrainMounts(
		ctx,
		decisions,
		childReports,
		parentAckers,
	); finalizeErr != nil {
		c.orchestrator.logger.Warn("finalizing drained shortcut child mounts",
			slog.String("parent_mount_id", parentID.String()),
			slog.String("error", finalizeErr.Error()),
		)
	} else if finalized {
		c.orchestrator.logger.Info("finalized drained shortcut child mounts",
			slog.String("parent_mount_id", parentID.String()),
		)
	}
}

func (c *oneShotChildRuns) waitParentDone(parentID mountID) {
	c.mu.Lock()
	done := c.parentDone[parentID]
	c.mu.Unlock()
	if done != nil {
		<-done
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
