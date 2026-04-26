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
) syncengine.ShortcutChildTopologySink {
	if o == nil || mount == nil || mount.projectionKind != MountProjectionStandalone {
		return nil
	}

	parent := *mount
	return func(_ context.Context, publication syncengine.ShortcutChildTopologyPublication) error {
		changed := o.receiveParentShortcutTopology(&parent, publication)
		if changed && restartOnChange {
			return syncengine.ErrMountTopologyChanged
		}
		return nil
	}
}

type shortcutTopologyHandlerSetter interface {
	SetShortcutTopologyHandler(syncengine.ShortcutChildTopologySink)
}

func setShortcutTopologyHandler(engine engineRunner, handler syncengine.ShortcutChildTopologySink) {
	if setter, ok := engine.(shortcutTopologyHandlerSetter); ok {
		setter.SetShortcutTopologyHandler(handler)
	}
}

func (o *Orchestrator) receiveParentShortcutTopology(
	parent *mountSpec,
	publication syncengine.ShortcutChildTopologyPublication,
) bool {
	if parent == nil {
		return false
	}
	if publication.NamespaceID == "" {
		publication.NamespaceID = parent.mountID.String()
	}
	if publication.NamespaceID != parent.mountID.String() {
		if o != nil && o.logger != nil {
			o.logger.Warn("ignoring parent shortcut topology from mismatched namespace",
				"namespace_id", publication.NamespaceID,
				"parent_id", parent.mountID.String(),
			)
		}
		return false
	}

	changed := o.storeParentShortcutTopology(parent.mountID, publication)
	if changed && o != nil && o.logger != nil {
		o.logger.Info("received parent shortcut topology",
			"namespace_id", parent.mountID.String(),
			"children", len(publication.Children),
		)
	}
	return changed
}
