package multisync

import (
	"context"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type parentChildWorkNotify func(context.Context, mountID) error

func (o *Orchestrator) attachParentChildWorkSink(
	mount *mountSpec,
	watchEvents chan<- watchRunnerEvent,
	notify parentChildWorkNotify,
) {
	if mount == nil {
		return
	}
	if mount.parent == nil {
		return
	}
	mount.parent.childWorkSink = o.parentChildWorkSinkForMount(mount, watchEvents, notify)
}

func (o *Orchestrator) parentChildWorkSinkForMount(
	mount *mountSpec,
	watchEvents chan<- watchRunnerEvent,
	notify parentChildWorkNotify,
) syncengine.ShortcutChildWorkSink {
	if o == nil || mount == nil || mount.projectionKind() != MountProjectionStandalone {
		return nil
	}

	parent := *mount
	return func(ctx context.Context, snapshot syncengine.ShortcutChildWorkSnapshot) error {
		changed := o.receiveParentChildWorkSnapshotFromParent(&parent, snapshot)
		if changed && watchEvents != nil {
			select {
			case watchEvents <- watchRunnerEvent{mountID: parent.id(), parentSnapshotChanged: true}:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if notify != nil {
			if err := notify(ctx, parent.id()); err != nil {
				return err
			}
		}
		return nil
	}
}

func (o *Orchestrator) receiveParentChildWorkSnapshotFromParent(
	parent *mountSpec,
	snapshot syncengine.ShortcutChildWorkSnapshot,
) bool {
	if parent == nil {
		return false
	}
	if snapshot.NamespaceID == "" {
		snapshot.NamespaceID = parent.id().String()
	}
	if snapshot.NamespaceID != parent.id().String() {
		if o != nil && o.logger != nil {
			o.logger.Warn("ignoring parent child work snapshot from mismatched namespace",
				"namespace_id", snapshot.NamespaceID,
				"parent_id", parent.id().String(),
			)
		}
		return false
	}

	changed := o.receiveParentChildWorkSnapshot(parent.id(), snapshot)
	if changed && o != nil && o.logger != nil {
		o.logger.Info("received parent child work snapshot",
			"namespace_id", parent.id().String(),
			"children", len(snapshot.RunCommands),
		)
	}
	return changed
}
