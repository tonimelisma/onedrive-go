package multisync

import (
	"context"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type parentChildWorkNotify func(context.Context, mountID, syncengine.ShortcutChildWorkSnapshot) error

// attachParentChildWorkSink points a parent mount's child-work publications at
// the run's snapshot cache. In watch mode each changed snapshot also becomes a
// runner event stamped with generation, the generation of the runner about to
// start for mount; one-shot passes no event channel and generation 0.
func (o *Orchestrator) attachParentChildWorkSink(
	mount *mountSpec,
	generation uint64,
	cache *parentChildWorkSnapshots,
	watchEvents chan<- watchRunnerEvent,
	notify parentChildWorkNotify,
) {
	if mount == nil {
		return
	}
	if mount.parent == nil {
		return
	}
	mount.parent.childWorkSink = o.parentChildWorkSinkForMount(mount, generation, cache, watchEvents, notify)
}

func (o *Orchestrator) parentChildWorkSinkForMount(
	mount *mountSpec,
	generation uint64,
	cache *parentChildWorkSnapshots,
	watchEvents chan<- watchRunnerEvent,
	notify parentChildWorkNotify,
) syncengine.ShortcutChildWorkSink {
	if o == nil || mount == nil || mount.projectionKind() != MountProjectionStandalone {
		return nil
	}

	parent := *mount
	return func(ctx context.Context, snapshot syncengine.ShortcutChildWorkSnapshot) error {
		normalized, changed := o.receiveParentChildWorkSnapshotFromParent(cache, &parent, snapshot)
		if changed && watchEvents != nil {
			select {
			case watchEvents <- watchRunnerEvent{
				mountID:               parent.id(),
				generation:            generation,
				parentSnapshot:        normalized,
				parentSnapshotChanged: true,
			}:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if notify != nil {
			if err := notify(ctx, parent.id(), normalized); err != nil {
				return err
			}
		}
		return nil
	}
}

func (o *Orchestrator) receiveParentChildWorkSnapshotFromParent(
	cache *parentChildWorkSnapshots,
	parent *mountSpec,
	snapshot syncengine.ShortcutChildWorkSnapshot,
) (syncengine.ShortcutChildWorkSnapshot, bool) {
	if parent == nil {
		return syncengine.ShortcutChildWorkSnapshot{}, false
	}
	if snapshot.NamespaceID == "" {
		snapshot.NamespaceID = parent.id().String()
	}
	if snapshot.NamespaceID != parent.id().String() {
		if o != nil && o.logger != nil {
			// An internal invariant the user can do nothing about, and it would
			// repeat on every publish from this parent; the snapshot is
			// dropped, so no wrong child work can start from it.
			o.logger.Debug("ignoring parent child work snapshot from mismatched namespace",
				"namespace_id", snapshot.NamespaceID,
				"parent_id", parent.id().String(),
			)
		}
		return syncengine.ShortcutChildWorkSnapshot{}, false
	}

	normalized, changed := cache.receive(parent.id(), snapshot)
	if changed && o != nil && o.logger != nil {
		o.logger.Info("received parent child work snapshot",
			"namespace_id", parent.id().String(),
			"children", len(normalized.RunCommands),
		)
	}
	return normalized, changed
}
