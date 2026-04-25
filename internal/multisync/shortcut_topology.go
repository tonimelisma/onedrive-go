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

//nolint:gocritic // ShortcutTopologyBatch is the sync API value type; this method is the handler boundary.
func (o *Orchestrator) applyShortcutTopologyBatch(
	_ context.Context,
	parent *mountSpec,
	batch syncengine.ShortcutTopologyBatch,
) (bool, error) {
	if parent == nil {
		return false, nil
	}
	if !batch.ShouldApply() {
		return false, nil
	}
	if batch.NamespaceID == "" {
		batch.NamespaceID = parent.mountID.String()
	}
	if batch.NamespaceID != parent.mountID.String() {
		return false, fmt.Errorf("shortcut topology namespace %q does not match parent %q", batch.NamespaceID, parent.mountID)
	}
	batch = parentDeclaredShortcutTopologyBatch(batch)

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

//nolint:gocritic // Keep the conversion value-oriented because callers need an immutable derived batch.
func parentDeclaredShortcutTopologyBatch(batch syncengine.ShortcutTopologyBatch) syncengine.ShortcutTopologyBatch {
	if len(batch.ParentRoots) == 0 {
		return batch
	}

	declared := syncengine.ShortcutTopologyBatch{
		NamespaceID: batch.NamespaceID,
		Kind:        syncengine.ShortcutTopologyObservationComplete,
	}
	for i := range batch.ParentRoots {
		root := batch.ParentRoots[i]
		switch root.State {
		case syncengine.ShortcutRootStateActive:
			declared.Upserts = append(declared.Upserts, syncengine.ShortcutBindingUpsert{
				BindingItemID:     root.BindingItemID,
				RelativeLocalPath: root.RelativeLocalPath,
				LocalAlias:        root.LocalAlias,
				RemoteDriveID:     root.RemoteDriveID.String(),
				RemoteItemID:      root.RemoteItemID,
				RemoteIsFolder:    root.RemoteIsFolder,
				Complete:          true,
			})
		case syncengine.ShortcutRootStateRemovedFinalDrain,
			syncengine.ShortcutRootStateRemovedCleanupBlocked:
			declared.Deletes = append(declared.Deletes, syncengine.ShortcutBindingDelete{
				BindingItemID: root.BindingItemID,
			})
		case syncengine.ShortcutRootStateSamePathReplacementWaiting:
			declared.Deletes = append(declared.Deletes, syncengine.ShortcutBindingDelete{
				BindingItemID: root.BindingItemID,
			})
			if root.Waiting != nil {
				declared.Upserts = append(declared.Upserts, syncengine.ShortcutBindingUpsert{
					BindingItemID:     root.Waiting.BindingItemID,
					RelativeLocalPath: root.Waiting.RelativeLocalPath,
					LocalAlias:        root.Waiting.LocalAlias,
					RemoteDriveID:     root.Waiting.RemoteDriveID.String(),
					RemoteItemID:      root.Waiting.RemoteItemID,
					RemoteIsFolder:    root.Waiting.RemoteIsFolder,
					Complete:          true,
				})
			}
		case syncengine.ShortcutRootStateTargetUnavailable,
			syncengine.ShortcutRootStateBlockedPath,
			syncengine.ShortcutRootStateRenameAmbiguous,
			syncengine.ShortcutRootStateAliasMutationBlocked:
			declared.Unavailable = append(declared.Unavailable, syncengine.ShortcutBindingUnavailable{
				BindingItemID:     root.BindingItemID,
				RelativeLocalPath: root.RelativeLocalPath,
				LocalAlias:        root.LocalAlias,
				RemoteDriveID:     root.RemoteDriveID.String(),
				RemoteItemID:      root.RemoteItemID,
				RemoteIsFolder:    root.RemoteIsFolder,
				Reason:            string(root.State),
			})
		default:
			declared.Unavailable = append(declared.Unavailable, syncengine.ShortcutBindingUnavailable{
				BindingItemID:     root.BindingItemID,
				RelativeLocalPath: root.RelativeLocalPath,
				LocalAlias:        root.LocalAlias,
				RemoteDriveID:     root.RemoteDriveID.String(),
				RemoteItemID:      root.RemoteItemID,
				RemoteIsFolder:    root.RemoteIsFolder,
				Reason:            string(root.State),
			})
		}
	}

	return declared
}

//nolint:gocritic // Tests and handlers pass topology batches as values to keep mutation explicit.
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
