package multisync

import (
	"context"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type parentRunnerPublicationNotify func(context.Context, mountID) error

func (o *Orchestrator) attachParentRunnerPublicationSink(
	mount *mountSpec,
	watchEvents chan<- watchRunnerEvent,
	notify parentRunnerPublicationNotify,
) {
	if mount == nil {
		return
	}
	mount.parentRunnerPublicationSink = o.parentRunnerPublicationSinkForMount(mount, watchEvents, notify)
}

func (o *Orchestrator) parentRunnerPublicationSinkForMount(
	mount *mountSpec,
	watchEvents chan<- watchRunnerEvent,
	notify parentRunnerPublicationNotify,
) syncengine.ShortcutChildRunnerSink {
	if o == nil || mount == nil || mount.projectionKind != MountProjectionStandalone {
		return nil
	}

	parent := *mount
	return func(ctx context.Context, publication syncengine.ShortcutChildRunnerPublication) error {
		changed := o.receiveParentRunnerPublicationFromParent(&parent, publication)
		if changed && watchEvents != nil {
			select {
			case watchEvents <- watchRunnerEvent{mountID: parent.mountID, parentPublicationChanged: true}:
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

func (o *Orchestrator) receiveParentRunnerPublicationFromParent(
	parent *mountSpec,
	publication syncengine.ShortcutChildRunnerPublication,
) bool {
	if parent == nil {
		return false
	}
	if publication.NamespaceID == "" {
		publication.NamespaceID = parent.mountID.String()
	}
	if publication.NamespaceID != parent.mountID.String() {
		if o != nil && o.logger != nil {
			o.logger.Warn("ignoring parent runner publication from mismatched namespace",
				"namespace_id", publication.NamespaceID,
				"parent_id", parent.mountID.String(),
			)
		}
		return false
	}

	changed := o.receiveParentRunnerPublication(parent.mountID, publication)
	if changed && o != nil && o.logger != nil {
		o.logger.Info("received parent runner publication",
			"namespace_id", parent.mountID.String(),
			"children", len(publication.RunnerWork.Children),
		)
	}
	return changed
}
