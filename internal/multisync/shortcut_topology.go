package multisync

import (
	"context"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type parentChildProcessNotify func(context.Context, mountID) error

func (o *Orchestrator) attachParentChildProcessSink(
	mount *mountSpec,
	watchEvents chan<- watchRunnerEvent,
	notify parentChildProcessNotify,
) {
	if mount == nil {
		return
	}
	mount.parentChildProcessSink = o.parentChildProcessSinkForMount(mount, watchEvents, notify)
}

func (o *Orchestrator) parentChildProcessSinkForMount(
	mount *mountSpec,
	watchEvents chan<- watchRunnerEvent,
	notify parentChildProcessNotify,
) syncengine.ShortcutChildProcessSink {
	if o == nil || mount == nil || mount.projectionKind != MountProjectionStandalone {
		return nil
	}

	parent := *mount
	return func(ctx context.Context, snapshot syncengine.ShortcutChildProcessSnapshot) error {
		changed := o.receiveParentChildProcessSnapshotFromParent(&parent, snapshot)
		if changed && watchEvents != nil {
			select {
			case watchEvents <- watchRunnerEvent{mountID: parent.mountID, parentSnapshotChanged: true}:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if notify != nil {
			if err := notify(ctx, parent.mountID); err != nil {
				return err
			}
		}
		return nil
	}
}

func (o *Orchestrator) receiveParentChildProcessSnapshotFromParent(
	parent *mountSpec,
	snapshot syncengine.ShortcutChildProcessSnapshot,
) bool {
	if parent == nil {
		return false
	}
	if snapshot.NamespaceID == "" {
		snapshot.NamespaceID = parent.mountID.String()
	}
	if snapshot.NamespaceID != parent.mountID.String() {
		if o != nil && o.logger != nil {
			o.logger.Warn("ignoring parent child process snapshot from mismatched namespace",
				"namespace_id", snapshot.NamespaceID,
				"parent_id", parent.mountID.String(),
			)
		}
		return false
	}

	changed := o.receiveParentChildProcessSnapshot(parent.mountID, snapshot)
	if changed && o != nil && o.logger != nil {
		o.logger.Info("received parent child process snapshot",
			"namespace_id", parent.mountID.String(),
			"children", len(snapshot.RunCommands),
		)
	}
	return changed
}
