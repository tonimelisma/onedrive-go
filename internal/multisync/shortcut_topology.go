package multisync

import (
	"context"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func (o *Orchestrator) attachShortcutTopologyHandler(mount *mountSpec, restartOnChange bool) {
	if mount == nil {
		return
	}
	mount.shortcutTopologyHandler = o.shortcutTopologyHandlerForMount(mount, restartOnChange)
}

func (o *Orchestrator) shortcutTopologyHandlerForMount(
	mount *mountSpec,
	restartOnChange bool,
) syncengine.ShortcutTopologyHandler {
	if o == nil || mount == nil || mount.projectionKind != MountProjectionStandalone {
		return nil
	}

	parent := *mount
	return func(_ context.Context, batch syncengine.ShortcutTopologyBatch) error {
		changed := o.applyShortcutTopologyBatch(&parent, batch)
		if changed && restartOnChange {
			return syncengine.ErrMountTopologyChanged
		}
		return nil
	}
}

type shortcutTopologyHandlerSetter interface {
	SetShortcutTopologyHandler(syncengine.ShortcutTopologyHandler)
}

func setShortcutTopologyHandler(engine engineRunner, handler syncengine.ShortcutTopologyHandler) {
	if setter, ok := engine.(shortcutTopologyHandlerSetter); ok {
		setter.SetShortcutTopologyHandler(handler)
	}
}

//nolint:gocritic // ShortcutTopologyBatch is the sync API value type; this method is the handler boundary.
func (o *Orchestrator) applyShortcutTopologyBatch(
	parent *mountSpec,
	batch syncengine.ShortcutTopologyBatch,
) bool {
	if parent == nil || !batch.ShouldApply() {
		return false
	}
	if batch.NamespaceID == "" {
		batch.NamespaceID = parent.mountID.String()
	}
	if batch.NamespaceID != parent.mountID.String() {
		if o != nil && o.logger != nil {
			o.logger.Warn("ignoring shortcut topology from mismatched namespace",
				"namespace_id", batch.NamespaceID,
				"parent_id", parent.mountID.String(),
			)
		}
		return false
	}

	changed := o.storeShortcutChildTopology(parent.mountID, batch.ChildTopologySnapshot())
	if changed && o != nil && o.logger != nil {
		o.logger.Info("applied shortcut topology batch",
			"namespace_id", parent.mountID.String(),
			"children", len(batch.ParentRoots),
		)
	}
	return changed
}
