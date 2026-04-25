package multisync

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/config"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func (o *Orchestrator) attachShortcutTopologyHandler(mount *mountSpec, restartOnChange bool) {
	if o == nil || mount == nil || mount.projectionKind != MountProjectionStandalone {
		return
	}

	parent := *mount
	mount.shortcutTopologyHandler = func(ctx context.Context, batch syncengine.ShortcutTopologyBatch) error {
		changed, err := o.applyShortcutTopologyBatch(ctx, &parent, batch)
		if err != nil {
			return err
		}
		if changed && restartOnChange {
			return syncengine.ErrMountTopologyChanged
		}
		return nil
	}
}

func (o *Orchestrator) applyShortcutTopologyBatch(
	_ context.Context,
	parent *mountSpec,
	batch syncengine.ShortcutTopologyBatch,
) (bool, error) {
	if parent == nil || !batch.HasFacts() {
		return false, nil
	}
	if batch.NamespaceID == "" {
		batch.NamespaceID = parent.mountID.String()
	}
	if batch.NamespaceID != parent.mountID.String() {
		return false, fmt.Errorf("shortcut topology namespace %q does not match parent %q", batch.NamespaceID, parent.mountID)
	}

	changed := false
	if err := config.UpdateMountInventory(func(inventory *config.MountInventory) error {
		existingByBinding := existingBindingsForNamespace(inventory, parent.mountID)
		result, err := applyShortcutTopologyBatchToInventory(inventory, parent, existingByBinding, batch)
		if err != nil {
			return err
		}
		changed = result.changed
		return nil
	}); err != nil {
		return false, fmt.Errorf("applying shortcut topology batch: %w", err)
	}
	if changed && o != nil && o.logger != nil {
		o.logger.Info("applied shortcut topology batch",
			"namespace_id", parent.mountID.String(),
			"upserts", len(batch.Upserts),
			"deletes", len(batch.Deletes),
			"unavailable", len(batch.Unavailable),
		)
	}

	return changed, nil
}

func applyShortcutTopologyBatchToInventory(
	inventory *config.MountInventory,
	parent *mountSpec,
	existingByBinding map[string]config.MountRecord,
	batch syncengine.ShortcutTopologyBatch,
) (mountInventoryMutationResult, error) {
	result := mountInventoryMutationResult{}

	result.merge(applyShortcutTopologyDeletes(inventory, existingByBinding, batch.Deletes))
	unavailableResult, err := applyShortcutTopologyBindings(
		inventory,
		parent,
		existingByBinding,
		unavailableTopologyBindings(batch.Unavailable),
	)
	if err != nil {
		return mountInventoryMutationResult{}, err
	}
	result.merge(unavailableResult)

	upsertResult, err := applyShortcutTopologyBindings(
		inventory,
		parent,
		existingByBinding,
		upsertTopologyBindings(batch.Upserts),
	)
	if err != nil {
		return mountInventoryMutationResult{}, err
	}
	result.merge(upsertResult)

	if batch.Kind == syncengine.ShortcutTopologyObservationComplete {
		seen := make(map[string]struct{}, len(batch.Upserts)+len(batch.Unavailable))
		for i := range batch.Upserts {
			seen[batch.Upserts[i].BindingItemID] = struct{}{}
		}
		for i := range batch.Unavailable {
			seen[batch.Unavailable[i].BindingItemID] = struct{}{}
		}
		for bindingID := range existingByBinding {
			if _, ok := seen[bindingID]; ok {
				continue
			}
			record := existingByBinding[bindingID]
			if markMountPendingRemoval(inventory, &record) {
				result.changed = true
				result.dirtyMountIDs = appendUniqueStrings(result.dirtyMountIDs, record.MountID)
			}
		}
	}

	return result, nil
}

func applyShortcutTopologyDeletes(
	inventory *config.MountInventory,
	existingByBinding map[string]config.MountRecord,
	deletes []syncengine.ShortcutBindingDelete,
) mountInventoryMutationResult {
	result := mountInventoryMutationResult{}
	for i := range deletes {
		record, found := existingByBinding[deletes[i].BindingItemID]
		if !found {
			continue
		}
		if markMountPendingRemoval(inventory, &record) {
			result.changed = true
			result.dirtyMountIDs = appendUniqueStrings(result.dirtyMountIDs, record.MountID)
		}
		delete(existingByBinding, deletes[i].BindingItemID)
	}
	return result
}

func applyShortcutTopologyBindings(
	inventory *config.MountInventory,
	parent *mountSpec,
	existingByBinding map[string]config.MountRecord,
	bindings []discoveredShortcutBinding,
) (mountInventoryMutationResult, error) {
	result := mountInventoryMutationResult{}
	for i := range bindings {
		updated, dirtyMountID, err := upsertChildMountRecord(inventory, parent, existingByBinding, bindings[i])
		if err != nil {
			return mountInventoryMutationResult{}, err
		}
		if updated {
			result.changed = true
			result.dirtyMountIDs = appendUniqueStrings(result.dirtyMountIDs, dirtyMountID)
		}
	}
	return result, nil
}

func unavailableTopologyBindings(facts []syncengine.ShortcutBindingUnavailable) []discoveredShortcutBinding {
	bindings := make([]discoveredShortcutBinding, 0, len(facts))
	for i := range facts {
		bindings = append(bindings, discoveredShortcutBinding{
			BindingItemID:     facts[i].BindingItemID,
			LocalAlias:        facts[i].LocalAlias,
			RelativeLocalPath: facts[i].RelativeLocalPath,
			RemoteDriveID:     facts[i].RemoteDriveID,
			RemoteItemID:      facts[i].RemoteItemID,
			State:             config.MountStateUnavailable,
			StateReason:       config.MountStateReasonShortcutBindingUnavailable,
		})
	}
	return bindings
}

func upsertTopologyBindings(facts []syncengine.ShortcutBindingUpsert) []discoveredShortcutBinding {
	bindings := make([]discoveredShortcutBinding, 0, len(facts))
	for i := range facts {
		bindings = append(bindings, discoveredShortcutBinding{
			BindingItemID:     facts[i].BindingItemID,
			LocalAlias:        facts[i].LocalAlias,
			RelativeLocalPath: facts[i].RelativeLocalPath,
			RemoteDriveID:     facts[i].RemoteDriveID,
			RemoteItemID:      facts[i].RemoteItemID,
		})
	}
	return bindings
}
