package multisync

import (
	"fmt"
	"reflect"
	"sort"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

type discoveredShortcutBinding struct {
	BindingItemID     string
	LocalAlias        string
	RelativeLocalPath string
	RemoteDriveID     string
	RemoteItemID      string
	State             config.MountState
	StateReason       config.MountStateReason
}

type mountInventoryMutationResult struct {
	changed          bool
	dirtyMountIDs    []string
	localRootActions []childRootLifecycleAction
}

func (r *mountInventoryMutationResult) merge(other mountInventoryMutationResult) {
	if r == nil {
		return
	}
	r.changed = r.changed || other.changed
	r.dirtyMountIDs = appendUniqueStrings(r.dirtyMountIDs, other.dirtyMountIDs...)
	r.localRootActions = append(r.localRootActions, other.localRootActions...)
}

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
	if !found {
		if owner, owned := siblingPathOwner(inventory, parent.mountID.String(), next.RelativeLocalPath, next.MountID); owned {
			plan, err := planDeferredShortcutReplacement(&owner)
			if err != nil {
				return false, "", err
			}
			if plan.Changed {
				inventory.Mounts[plan.Record.MountID] = plan.Record
			}
			recordDeferredShortcutBinding(inventory, parent, &next)
			return true, owner.MountID, nil
		}
	}

	if found && reflect.DeepEqual(next, record) {
		return false, "", nil
	}

	if found && record.MountID != next.MountID {
		delete(inventory.Mounts, record.MountID)
	}
	inventory.Mounts[next.MountID] = next
	existingByBinding[binding.BindingItemID] = next
	deleteDeferredShortcutBinding(inventory, next.NamespaceID, next.RelativeLocalPath, next.BindingItemID)
	return true, next.MountID, nil
}

func siblingPathOwner(
	inventory *config.MountInventory,
	namespaceID string,
	relativeLocalPath string,
	nextMountID string,
) (config.MountRecord, bool) {
	if inventory == nil || relativeLocalPath == "" {
		return config.MountRecord{}, false
	}
	for mountID := range inventory.Mounts {
		record := inventory.Mounts[mountID]
		if record.NamespaceID != namespaceID || record.MountID == nextMountID {
			continue
		}
		if record.RelativeLocalPath == relativeLocalPath {
			return record, true
		}
		for _, reservedPath := range record.ReservedLocalPaths {
			if reservedPath == relativeLocalPath {
				return record, true
			}
		}
	}
	return config.MountRecord{}, false
}

func recordDeferredShortcutBinding(
	inventory *config.MountInventory,
	parent *mountSpec,
	record *config.MountRecord,
) {
	if inventory == nil || parent == nil || record == nil || record.BindingItemID == "" || record.RelativeLocalPath == "" {
		return
	}
	if inventory.DeferredShortcutBindings == nil {
		inventory.DeferredShortcutBindings = make(map[string]config.DeferredShortcutBinding)
	}
	key := deferredShortcutBindingKey(record.NamespaceID, record.RelativeLocalPath, record.BindingItemID)
	inventory.DeferredShortcutBindings[key] = config.DeferredShortcutBinding{
		NamespaceID:         record.NamespaceID,
		BindingItemID:       record.BindingItemID,
		LocalAlias:          record.LocalAlias,
		RelativeLocalPath:   record.RelativeLocalPath,
		TokenOwnerCanonical: parent.tokenOwnerCanonical.String(),
		RemoteDriveID:       record.RemoteDriveID,
		RemoteItemID:        record.RemoteItemID,
		State:               record.State,
		StateReason:         record.StateReason,
	}
}

func deleteDeferredShortcutBinding(
	inventory *config.MountInventory,
	namespaceID string,
	relativeLocalPath string,
	bindingItemID string,
) {
	if inventory == nil || len(inventory.DeferredShortcutBindings) == 0 {
		return
	}
	delete(inventory.DeferredShortcutBindings, deferredShortcutBindingKey(namespaceID, relativeLocalPath, bindingItemID))
}

func deferredShortcutBindingKey(namespaceID string, relativeLocalPath string, bindingItemID string) string {
	return namespaceID + "|" + relativeLocalPath + "|" + bindingItemID
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

	plan, err := planShortcutPendingRemoval(record)
	if err != nil || !plan.Changed {
		return false
	}
	inventory.Mounts[record.MountID] = plan.Record
	*record = plan.Record
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
		reason := config.MountStateReason("")
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
	reason config.MountStateReason,
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
		if record.State == config.MountStatePendingRemoval &&
			record.StateReason != config.MountStateReasonShortcutRemoved {
			ids = append(ids, record.MountID)
		}
	}

	return ids
}

func finalDrainMountIDs(inventory *config.MountInventory) []string {
	if inventory == nil {
		return nil
	}

	ids := make([]string, 0)
	records := sortedMountRecords(inventory)
	for i := range records {
		record := &records[i]
		if record.State == config.MountStatePendingRemoval &&
			record.StateReason == config.MountStateReasonShortcutRemoved {
			ids = append(ids, record.MountID)
		}
	}

	return ids
}

func promoteDeferredShortcutBindings(
	inventory *config.MountInventory,
	parents []*mountSpec,
) mountInventoryMutationResult {
	if inventory == nil || len(inventory.DeferredShortcutBindings) == 0 {
		return mountInventoryMutationResult{}
	}

	parentByID := make(map[string]*mountSpec, len(parents))
	for i := range parents {
		parentByID[parents[i].mountID.String()] = parents[i]
	}

	result := mountInventoryMutationResult{}
	for key := range inventory.DeferredShortcutBindings {
		deferred := inventory.DeferredShortcutBindings[key]
		parent := parentByID[deferred.NamespaceID]
		if parent == nil {
			continue
		}
		if _, owned := siblingPathOwner(inventory, deferred.NamespaceID, deferred.RelativeLocalPath, ""); owned {
			continue
		}

		binding := discoveredShortcutBinding{
			BindingItemID:     deferred.BindingItemID,
			LocalAlias:        deferred.LocalAlias,
			RelativeLocalPath: deferred.RelativeLocalPath,
			RemoteDriveID:     deferred.RemoteDriveID,
			RemoteItemID:      deferred.RemoteItemID,
			State:             deferred.State,
			StateReason:       deferred.StateReason,
		}
		existingByBinding := existingBindingsForNamespace(inventory, parent.mountID)
		updated, dirtyMountID, err := upsertChildMountRecord(inventory, parent, existingByBinding, binding)
		if err != nil {
			continue
		}
		if updated {
			result.changed = true
			result.dirtyMountIDs = appendUniqueStrings(result.dirtyMountIDs, dirtyMountID)
		}
		delete(inventory.DeferredShortcutBindings, key)
	}

	return result
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
