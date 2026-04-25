package multisync

import (
	"context"
	"fmt"
	"reflect"
	"sort"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func existingBindingsForNamespace(inventory *config.MountInventory, namespaceID mountID) map[string]config.MountRecord {
	byBinding := make(map[string]config.MountRecord)
	for mountID := range inventory.Mounts {
		record := inventory.Mounts[mountID]
		if record.NamespaceID != namespaceID.String() || record.BindingItemID == "" {
			continue
		}
		byBinding[record.BindingItemID] = record
	}
	return byBinding
}

func applyFullParentEnumeration(
	inventory *config.MountInventory,
	parent *mountSpec,
	existingByBinding map[string]config.MountRecord,
	discovered map[string]discoveredShortcutBinding,
) (parentReconcileResult, error) {
	changed := false
	removed := make([]string, 0)
	dirtyMountIDs := make([]string, 0)
	for bindingID, binding := range discovered {
		updated, dirtyMountID, err := upsertChildMountRecord(inventory, parent, existingByBinding, binding)
		if err != nil {
			return parentReconcileResult{}, err
		}
		changed = changed || updated
		if updated {
			dirtyMountIDs = appendUniqueStrings(dirtyMountIDs, dirtyMountID)
		}
		delete(existingByBinding, bindingID)
	}

	for bindingID := range existingByBinding {
		record := existingByBinding[bindingID]
		updated := markMountPendingRemoval(inventory, &record)
		delete(existingByBinding, bindingID)
		if updated {
			removed = append(removed, record.MountID)
			changed = true
			dirtyMountIDs = appendUniqueStrings(dirtyMountIDs, record.MountID)
		}
	}

	return parentReconcileResult{changed: changed, removedMountIDs: removed, dirtyMountIDs: dirtyMountIDs}, nil
}

func applyIncrementalParentDelta(
	ctx context.Context,
	inventory *config.MountInventory,
	existingByBinding map[string]config.MountRecord,
	session *driveopsSessionView,
	parent *mountSpec,
	rootPathPrefix string,
	items []graph.Item,
) (parentReconcileResult, error) {
	changed := false
	removed := make([]string, 0)
	dirtyMountIDs := make([]string, 0)
	for i := range items {
		item := &items[i]
		if item.ID == "" {
			continue
		}
		record, found := existingByBinding[item.ID]
		if item.IsDeleted {
			if found {
				if markMountPendingRemoval(inventory, &record) {
					removed = append(removed, record.MountID)
					changed = true
					dirtyMountIDs = appendUniqueStrings(dirtyMountIDs, record.MountID)
				}
			}
			continue
		}

		binding, ok, err := resolveDiscoveredShortcutBinding(ctx, session, parent, rootPathPrefix, item, found)
		if err != nil {
			return parentReconcileResult{}, err
		}
		if !ok {
			if found {
				if markMountPendingRemoval(inventory, &record) {
					removed = append(removed, record.MountID)
					changed = true
					dirtyMountIDs = appendUniqueStrings(dirtyMountIDs, record.MountID)
				}
			}
			continue
		}

		updated, dirtyMountID, err := upsertChildMountRecord(inventory, parent, existingByBinding, binding)
		if err != nil {
			return parentReconcileResult{}, err
		}
		changed = changed || updated
		if updated {
			dirtyMountIDs = appendUniqueStrings(dirtyMountIDs, dirtyMountID)
		}
	}

	return parentReconcileResult{changed: changed, removedMountIDs: removed, dirtyMountIDs: dirtyMountIDs}, nil
}

func upsertChildMountRecord(
	inventory *config.MountInventory,
	parent *mountSpec,
	existingByBinding map[string]config.MountRecord,
	binding discoveredShortcutBinding,
) (bool, string, error) {
	record, found := existingByBinding[binding.BindingItemID]
	if !found {
		record = config.MountRecord{
			MountID:       config.ChildMountID(parent.mountID.String(), binding.BindingItemID),
			NamespaceID:   parent.mountID.String(),
			BindingItemID: binding.BindingItemID,
		}
	}

	if record.MountID == "" {
		return false, "", fmt.Errorf("constructing child mount ID for namespace %s binding %s", parent.mountID, binding.BindingItemID)
	}

	next := childMountRecordForBinding(parent, &record, found, &binding)
	if !found && next.RelativeLocalPath == "" {
		return false, "", nil
	}

	if found && reflect.DeepEqual(next, record) {
		return false, "", nil
	}

	if found && record.MountID != next.MountID {
		delete(inventory.Mounts, record.MountID)
	}
	inventory.Mounts[next.MountID] = next
	existingByBinding[binding.BindingItemID] = next
	return true, next.MountID, nil
}

func childMountRecordForBinding(
	parent *mountSpec,
	record *config.MountRecord,
	found bool,
	binding *discoveredShortcutBinding,
) config.MountRecord {
	next := *record
	next.NamespaceID = parent.mountID.String()
	next.BindingItemID = binding.BindingItemID
	next.TokenOwnerCanonical = parent.tokenOwnerCanonical.String()
	applyChildBindingPath(&next, record, found, binding)
	applyChildBindingRemote(&next, binding)
	applyChildBindingLifecycle(&next, binding)
	return next
}

func applyChildBindingPath(
	next *config.MountRecord,
	record *config.MountRecord,
	found bool,
	binding *discoveredShortcutBinding,
) {
	if binding.LocalAlias != "" {
		next.LocalAlias = binding.LocalAlias
	}
	if binding.RelativeLocalPath == "" {
		return
	}
	if found && record.RelativeLocalPath != "" && record.RelativeLocalPath != binding.RelativeLocalPath {
		next.ReservedLocalPaths = appendUniqueStrings(next.ReservedLocalPaths, record.RelativeLocalPath)
		next.LocalRootMaterialized = false
	}
	next.RelativeLocalPath = binding.RelativeLocalPath
}

func applyChildBindingRemote(next *config.MountRecord, binding *discoveredShortcutBinding) {
	if binding.RemoteDriveID != "" {
		next.RemoteDriveID = binding.RemoteDriveID
	}
	if binding.RemoteItemID != "" {
		next.RemoteItemID = binding.RemoteItemID
	}
}

func applyChildBindingLifecycle(next *config.MountRecord, binding *discoveredShortcutBinding) {
	if binding.State == config.MountStateUnavailable {
		next.State = config.MountStateUnavailable
		next.StateReason = binding.StateReason
		return
	}

	next.State = config.MountStateActive
	next.StateReason = ""
}

func markMountPendingRemoval(inventory *config.MountInventory, record *config.MountRecord) bool {
	if inventory == nil || record == nil || record.MountID == "" {
		return false
	}
	if record.State == config.MountStatePendingRemoval &&
		record.StateReason == config.MountStateReasonShortcutRemoved {
		return false
	}

	record.State = config.MountStatePendingRemoval
	record.StateReason = config.MountStateReasonShortcutRemoved
	inventory.Mounts[record.MountID] = *record
	return true
}

func applyDurableProjectionConflicts(inventory *config.MountInventory, parents []*mountSpec) mountInventoryMutationResult {
	if inventory == nil {
		return mountInventoryMutationResult{}
	}

	_, standaloneByRoot := indexStandaloneMounts(parents)
	recordsByNamespaceRoot := make(map[string][]config.MountRecord)
	result := mountInventoryMutationResult{}
	records := sortedMountRecords(inventory)
	for i := range records {
		record := &records[i]
		if record.State == config.MountStatePendingRemoval ||
			record.State == config.MountStateUnavailable {
			continue
		}
		key, err := contentRootKeyForRecord(record)
		if err != nil {
			continue
		}
		if _, found := standaloneByRoot[key]; found {
			if setMountLifecycleState(
				inventory,
				record,
				config.MountStateConflict,
				config.MountStateReasonExplicitStandaloneContentRoot,
			) {
				result.changed = true
				result.dirtyMountIDs = appendUniqueStrings(result.dirtyMountIDs, record.MountID)
			}
			continue
		}

		namespaceRootKey := record.NamespaceID + "|" + key
		recordsByNamespaceRoot[namespaceRootKey] = append(recordsByNamespaceRoot[namespaceRootKey], *record)
	}

	for _, records := range recordsByNamespaceRoot {
		sort.SliceStable(records, func(i, j int) bool {
			if records[i].RelativeLocalPath == records[j].RelativeLocalPath {
				return records[i].MountID < records[j].MountID
			}

			return records[i].RelativeLocalPath < records[j].RelativeLocalPath
		})

		state := config.MountStateActive
		reason := ""
		if len(records) > 1 {
			state = config.MountStateConflict
			reason = config.MountStateReasonDuplicateContentRoot
		}
		for i := range records {
			if setMountLifecycleState(inventory, &records[i], state, reason) {
				result.changed = true
				result.dirtyMountIDs = appendUniqueStrings(result.dirtyMountIDs, records[i].MountID)
			}
		}
	}

	return result
}

func setMountLifecycleState(
	inventory *config.MountInventory,
	record *config.MountRecord,
	state config.MountState,
	reason string,
) bool {
	if record == nil || record.State == state && record.StateReason == reason {
		return false
	}

	record.State = state
	record.StateReason = reason
	inventory.Mounts[record.MountID] = *record
	return true
}

func contentRootKeyForRecord(record *config.MountRecord) (string, error) {
	tokenOwner, err := driveid.NewCanonicalID(record.TokenOwnerCanonical)
	if err != nil {
		return "", fmt.Errorf("parsing mount %s token owner: %w", record.MountID, err)
	}

	return fmt.Sprintf("%s|%s|%s", tokenOwner.String(), driveid.New(record.RemoteDriveID).String(), record.RemoteItemID), nil
}

func pendingRemovalMountIDs(inventory *config.MountInventory) []string {
	if inventory == nil {
		return nil
	}

	ids := make([]string, 0)
	records := sortedMountRecords(inventory)
	for i := range records {
		record := &records[i]
		if record.State == config.MountStatePendingRemoval {
			ids = append(ids, record.MountID)
		}
	}

	return ids
}

func appendUniqueStrings(values []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(values)+len(additions))
	for _, value := range values {
		seen[value] = struct{}{}
	}
	for _, value := range additions {
		if value == "" {
			continue
		}
		if _, found := seen[value]; found {
			continue
		}
		values = append(values, value)
		seen[value] = struct{}{}
	}

	return values
}
