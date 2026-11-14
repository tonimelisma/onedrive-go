package multisync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	gosync "sync"

	"github.com/tonimelisma/onedrive-go/internal/panicguard"
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

	// mu guards every field below it: cache, parents, parentAckers,
	// parentDone, snapshots, started, startup, childReports and stuckCloses.
	// Parent and child mount goroutines all write here as they finish. Every
	// holder releases it with defer: a panic boundary of the pass reports
	// through it, and would wait forever on a lock a panic left held.
	mu           gosync.Mutex
	cache        *parentChildWorkSnapshots
	parents      map[mountID]*mountSpec
	parentAckers map[mountID]shortcutChildAckHandle
	parentDone   map[mountID]bool
	snapshots    map[mountID]syncengine.ShortcutChildWorkSnapshot
	started      map[mountID]bool
	startup      []MountStartupResult
	childReports []*MountReport
	// stuckCloses are the shortcut engines that did not close within their
	// bound or whose close panicked (closeRunOnceEngines), one joined error
	// per parent.
	stuckCloses   []error
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

	c.recordSnapshot(parentID, snapshot)
	c.startChildrenForParent(ctx, parentID)
	return nil
}

func (c *oneShotChildRuns) markParentDone(ctx context.Context, parentID mountID) {
	if c == nil || parentID == "" {
		return
	}
	c.recordParentDone(parentID)
	c.startChildrenForParent(ctx, parentID)
}

// recordSnapshot keeps the latest work snapshot parentID published.
func (c *oneShotChildRuns) recordSnapshot(parentID mountID, snapshot syncengine.ShortcutChildWorkSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshots[parentID] = snapshot
}

// recordParentDone marks parentID's own run finished.
func (c *oneShotChildRuns) recordParentDone(parentID mountID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.parentDone[parentID] = true
}

func (c *oneShotChildRuns) startChildrenForParent(ctx context.Context, parentID mountID) {
	if c == nil || parentID == "" {
		return
	}
	if ctx != nil && ctx.Err() != nil {
		return
	}
	parent, snapshot, claimed := c.claimChildRun(parentID)
	if !claimed {
		return
	}

	go c.runChildrenForParent(ctx, parentID, parent, snapshot)
}

// claimChildRun marks parentID's shortcuts started, once the parent has
// finished and published work, and counts their run in childRunGroup; the
// caller then starts that run. It reports false when there is nothing to
// start yet, or nothing at all.
func (c *oneShotChildRuns) claimChildRun(parentID mountID) (*mountSpec, syncengine.ShortcutChildWorkSnapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.started[parentID] || !c.parentDone[parentID] {
		return nil, syncengine.ShortcutChildWorkSnapshot{}, false
	}
	parent := cloneMountSpec(c.parents[parentID])
	if parent == nil {
		return nil, syncengine.ShortcutChildWorkSnapshot{}, false
	}
	snapshot := c.snapshots[parentID]
	if !shortcutChildWorkSnapshotHasWork(snapshot) {
		return nil, syncengine.ShortcutChildWorkSnapshot{}, false
	}
	c.started[parentID] = true
	c.childRunGroup.Add(1)

	return parent, snapshot, true
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

// appendStuck records the shortcut engines of one parent that did not close.
func (c *oneShotChildRuns) appendStuck(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stuckCloses = append(c.stuckCloses, err)
}

// stuck joins the errors of every shortcut engine that did not close.
func (c *oneShotChildRuns) stuck() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return errors.Join(c.stuckCloses...)
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
	// Each shortcut's own run recovers its panic into its report
	// (MountRunner). A panic in the work around them becomes a report against
	// the drive, as a failed cleanup does, so the pass still finishes and
	// fails; an engine it did not get to close is left to the process exit.
	defer func() {
		if r := recover(); r != nil {
			c.appendReports(parentMountReport(parent, panicguard.Recovered(c.orchestrator.logger,
				"while syncing the shortcuts of "+parent.label(), r)))
		}
	}()
	decisions, err := c.orchestrator.buildRuntimeWorkForParentSnapshot(parent, snapshot)
	if err != nil {
		c.appendStartup(MountStartupResult{
			SelectionIndex: parent.selectionIndex(),
			Identity:       parent.identity(),
			DisplayName:    parent.displayName(),
			Status:         MountStartupFatal,
			Err:            fmt.Errorf("set up shared-folder shortcuts: %w", err),
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
	runIndexedMountWork(ctx, childWork, childReports, nil)
	stuckErr := c.orchestrator.closeRunOnceEngines(ctx, childWork, MountProjectionChild)
	c.appendReports(childReports...)
	if stuckErr != nil {
		// A shortcut whose engine may still be writing its store gets no
		// cleanup and no final-drain acknowledgement: either would act on
		// that store. The next run retries both.
		c.appendStuck(stuckErr)
		return
	}

	parentAckers := c.parentAckersFor(parentID)
	if purgeErr := c.orchestrator.purgeShortcutChildArtifactsForDecisions(
		ctx,
		decisions,
		cloneParentAckHandles(parentAckers),
		c.cache,
	); purgeErr != nil {
		// The report is the user-facing channel for this failure; logging it
		// at WARN as well printed it twice.
		c.appendReports(parentMountReport(parent, fmt.Errorf("clean up removed shortcuts: %w", purgeErr)))
	}
	if finalized, finalizeErr := c.orchestrator.finalizeSuccessfulFinalDrainMounts(
		ctx,
		decisions,
		childReports,
		parentAckers,
		c.cache,
	); finalizeErr != nil {
		c.appendReports(parentMountReport(parent, fmt.Errorf("finish removing shortcuts: %w", finalizeErr)))
	} else if finalized {
		c.orchestrator.logger.Info("Finished removing shortcuts",
			slog.String("drive", parent.label()),
			slog.String("parent_mount_id", parentID.String()),
		)
	}
}

// parentMountReport is a report against parent for work of its shortcuts
// that failed outside any one shortcut's run.
func parentMountReport(parent *mountSpec, err error) *MountReport {
	return &MountReport{
		SelectionIndex: parent.selectionIndex(),
		Identity:       parent.identity(),
		DisplayName:    parent.displayName(),
		Err:            err,
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
	defer c.mu.Unlock()
	c.startup = append(c.startup, results...)
}

func (c *oneShotChildRuns) appendReports(reports ...*MountReport) {
	if c == nil || len(reports) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.childReports = append(c.childReports, reports...)
}
