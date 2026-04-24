package multisync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"reflect"
	"sort"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type discoveredShortcutBinding struct {
	BindingItemID     string
	LocalAlias        string
	RelativeLocalPath string
	RemoteDriveID     string
	RemoteItemID      string
	State             config.MountState
	StateReason       string
}

const (
	driveRemoteRootItemID     = "root"
	initialSeenFolderCapacity = 16
)

type parentReconcileResult struct {
	changed         bool
	removedMountIDs []string
}

type namespaceReconcileResult struct {
	inventory       *config.MountInventory
	removedMountIDs []string
}

type namespaceRuntime struct {
	cfg    *OrchestratorConfig
	logger *slog.Logger
}

func (o *Orchestrator) reconcileManagedShortcutMounts(
	ctx context.Context,
	parents []*mountSpec,
) (namespaceReconcileResult, error) {
	runtime := &namespaceRuntime{
		cfg:    o.cfg,
		logger: o.logger,
	}
	return runtime.reconcile(ctx, parents)
}

func (n *namespaceRuntime) reconcile(
	ctx context.Context,
	parents []*mountSpec,
) (namespaceReconcileResult, error) {
	inventory, err := config.LoadMountInventory()
	if err != nil {
		return namespaceReconcileResult{}, fmt.Errorf("loading mount inventory: %w", err)
	}

	result := namespaceReconcileResult{inventory: inventory}
	changed := false
	for _, parent := range parents {
		parentResult, parentErr := n.reconcileNamespaceMount(ctx, inventory, parent)
		if parentErr != nil {
			if n.logger != nil {
				n.logger.Warn("shortcut reconciliation failed",
					"namespace_id", parent.mountID.String(),
					"error", parentErr,
				)
			}
			continue
		}
		changed = changed || parentResult.changed
		result.removedMountIDs = append(result.removedMountIDs, parentResult.removedMountIDs...)
	}
	changed = applyDurableProjectionConflicts(inventory, parents) || changed
	result.removedMountIDs = appendUniqueStrings(result.removedMountIDs, pendingRemovalMountIDs(inventory)...)

	if !changed {
		return result, nil
	}

	if err := config.SaveMountInventory(inventory); err != nil {
		return namespaceReconcileResult{}, fmt.Errorf("saving mount inventory: %w", err)
	}

	return result, nil
}

func (n *namespaceRuntime) reconcileNamespaceMount(
	ctx context.Context,
	inventory *config.MountInventory,
	parent *mountSpec,
) (parentReconcileResult, error) {
	if inventory == nil || parent == nil {
		return parentReconcileResult{}, nil
	}

	session, err := n.cfg.Runtime.SyncSession(ctx, parent.syncSessionConfig())
	if err != nil {
		return parentReconcileResult{}, fmt.Errorf("session for namespace mount %s: %w", parent.mountID, err)
	}
	sessionView := newDriveopsSessionView(session)

	rootPathPrefix, err := mountedRootPathPrefix(ctx, session, parent)
	if err != nil {
		return parentReconcileResult{}, err
	}

	state := inventory.Namespaces[parent.mountID.String()]
	if state.NamespaceID == "" {
		state.NamespaceID = parent.mountID.String()
	}

	switch {
	case parent.remoteRootItemID == "":
		return n.reconcileNamespaceMountDelta(ctx, inventory, parent, sessionView, rootPathPrefix, state, false)
	case parent.remoteRootDeltaCapable:
		result, deltaErr := n.reconcileNamespaceMountDelta(ctx, inventory, parent, sessionView, rootPathPrefix, state, true)
		if deltaErr == nil {
			return result, nil
		}
		if errors.Is(deltaErr, graph.ErrMethodNotAllowed) || errors.Is(deltaErr, graph.ErrNotFound) {
			return n.reconcileNamespaceMountByListing(ctx, inventory, parent, sessionView, rootPathPrefix, state)
		}
		return parentReconcileResult{}, deltaErr
	default:
		return n.reconcileNamespaceMountByListing(ctx, inventory, parent, sessionView, rootPathPrefix, state)
	}
}

func (n *namespaceRuntime) reconcileNamespaceMountDelta(
	ctx context.Context,
	inventory *config.MountInventory,
	parent *mountSpec,
	session *driveopsSessionView,
	rootPathPrefix string,
	state config.NamespaceDiscoveryState,
	folderScoped bool,
) (parentReconcileResult, error) {
	var (
		items    []graph.Item
		newToken string
		err      error
	)

	fullEnumeration := state.DeltaLink == ""
	if folderScoped {
		items, newToken, err = session.meta.DeltaFolderAll(ctx, parent.remoteDriveID.String(), parent.remoteRootItemID, state.DeltaLink)
	} else {
		items, newToken, err = session.meta.DeltaAll(ctx, parent.remoteDriveID.String(), state.DeltaLink)
	}
	if err != nil {
		if errors.Is(err, graph.ErrGone) {
			changed := state.DeltaLink != "" || state.DiscoveryMode != config.DiscoveryModeDelta
			state.DeltaLink = ""
			state.DiscoveryMode = config.DiscoveryModeDelta
			inventory.Namespaces[parent.mountID.String()] = state
			return parentReconcileResult{changed: changed}, nil
		}
		return parentReconcileResult{}, fmt.Errorf("delta discovery for namespace mount %s: %w", parent.mountID, err)
	}

	state.DeltaLink = newToken
	state.DiscoveryMode = config.DiscoveryModeDelta
	inventory.Namespaces[parent.mountID.String()] = state

	existingByBinding := existingBindingsForNamespace(inventory, parent.mountID)
	if fullEnumeration {
		discovered := make(map[string]discoveredShortcutBinding)
		for i := range items {
			_, knownBinding := existingByBinding[items[i].ID]
			binding, ok, bindErr := resolveDiscoveredShortcutBinding(
				ctx,
				session,
				parent,
				rootPathPrefix,
				&items[i],
				knownBinding,
			)
			if bindErr != nil {
				return parentReconcileResult{}, bindErr
			}
			if !ok {
				continue
			}
			discovered[binding.BindingItemID] = binding
		}
		return applyFullParentEnumeration(inventory, parent, existingByBinding, discovered)
	}

	return applyIncrementalParentDelta(ctx, inventory, existingByBinding, session, parent, rootPathPrefix, items)
}

func (n *namespaceRuntime) reconcileNamespaceMountByListing(
	ctx context.Context,
	inventory *config.MountInventory,
	parent *mountSpec,
	session *driveopsSessionView,
	rootPathPrefix string,
	state config.NamespaceDiscoveryState,
) (parentReconcileResult, error) {
	existingByBinding := existingBindingsForNamespace(inventory, parent.mountID)
	bindings, err := enumerateShortcutBindings(ctx, session, parent, rootPathPrefix, existingByBinding)
	if err != nil {
		return parentReconcileResult{}, err
	}

	state.NamespaceID = parent.mountID.String()
	state.DeltaLink = ""
	state.DiscoveryMode = config.DiscoveryModeEnumerate
	inventory.Namespaces[parent.mountID.String()] = state

	changed := false
	for _, binding := range bindings {
		updated, err := upsertChildMountRecord(inventory, parent, existingByBinding, binding)
		if err != nil {
			return parentReconcileResult{}, err
		}
		changed = changed || updated
	}

	return parentReconcileResult{changed: changed}, nil
}

type driveopsSessionView struct {
	meta graphDiscoveryClient
}

type graphDiscoveryClient interface {
	DeltaAll(ctx context.Context, driveID string, token string) ([]graph.Item, string, error)
	DeltaFolderAll(ctx context.Context, driveID string, folderID, token string) ([]graph.Item, string, error)
	GetItem(ctx context.Context, driveID string, itemID string) (*graph.Item, error)
	ListChildren(ctx context.Context, driveID string, parentID string) ([]graph.Item, error)
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

func applyFullParentEnumeration(
	inventory *config.MountInventory,
	parent *mountSpec,
	existingByBinding map[string]config.MountRecord,
	discovered map[string]discoveredShortcutBinding,
) (parentReconcileResult, error) {
	changed := false
	removed := make([]string, 0)
	for bindingID, binding := range discovered {
		updated, err := upsertChildMountRecord(inventory, parent, existingByBinding, binding)
		if err != nil {
			return parentReconcileResult{}, err
		}
		changed = changed || updated
		delete(existingByBinding, bindingID)
	}

	for bindingID := range existingByBinding {
		record := existingByBinding[bindingID]
		updated := markMountPendingRemoval(inventory, &record)
		delete(existingByBinding, bindingID)
		if updated {
			removed = append(removed, record.MountID)
			changed = true
		}
	}

	return parentReconcileResult{changed: changed, removedMountIDs: removed}, nil
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
				}
			}
			continue
		}

		updated, err := upsertChildMountRecord(inventory, parent, existingByBinding, binding)
		if err != nil {
			return parentReconcileResult{}, err
		}
		changed = changed || updated
	}

	return parentReconcileResult{changed: changed, removedMountIDs: removed}, nil
}

func upsertChildMountRecord(
	inventory *config.MountInventory,
	parent *mountSpec,
	existingByBinding map[string]config.MountRecord,
	binding discoveredShortcutBinding,
) (bool, error) {
	record, found := existingByBinding[binding.BindingItemID]
	if !found {
		record = config.MountRecord{
			MountID:       config.ChildMountID(parent.mountID.String(), binding.BindingItemID),
			NamespaceID:   parent.mountID.String(),
			BindingItemID: binding.BindingItemID,
		}
	}

	if record.MountID == "" {
		return false, fmt.Errorf("constructing child mount ID for namespace %s binding %s", parent.mountID, binding.BindingItemID)
	}

	next := childMountRecordForBinding(parent, &record, found, &binding)
	if !found && next.RelativeLocalPath == "" {
		return false, nil
	}

	if found && reflect.DeepEqual(next, record) {
		return false, nil
	}

	if found && record.MountID != next.MountID {
		delete(inventory.Mounts, record.MountID)
	}
	inventory.Mounts[next.MountID] = next
	existingByBinding[binding.BindingItemID] = next
	return true, nil
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

func applyDurableProjectionConflicts(inventory *config.MountInventory, parents []*mountSpec) bool {
	if inventory == nil {
		return false
	}

	_, standaloneByRoot := indexStandaloneMounts(parents)
	recordsByNamespaceRoot := make(map[string][]config.MountRecord)
	changed := false
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
			changed = setMountLifecycleState(
				inventory,
				record,
				config.MountStateConflict,
				config.MountStateReasonExplicitStandaloneContentRoot,
			) || changed
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

		for i := range records {
			state := config.MountStateActive
			reason := ""
			if i > 0 {
				state = config.MountStateConflict
				reason = config.MountStateReasonDuplicateContentRoot
			}
			changed = setMountLifecycleState(inventory, &records[i], state, reason) || changed
		}
	}

	return changed
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

func enumerateShortcutBindings(
	ctx context.Context,
	session *driveopsSessionView,
	parent *mountSpec,
	rootPathPrefix string,
	existingByBinding map[string]config.MountRecord,
) ([]discoveredShortcutBinding, error) {
	remoteRootItemID := parent.remoteRootItemID
	if remoteRootItemID == "" {
		remoteRootItemID = driveRemoteRootItemID
	}

	queue := []string{remoteRootItemID}
	bindings := make([]discoveredShortcutBinding, 0)
	seenFolders := make(map[string]struct{}, initialSeenFolderCapacity)

	for len(queue) > 0 {
		folderID := queue[0]
		queue = queue[1:]
		if _, seen := seenFolders[folderID]; seen {
			continue
		}
		seenFolders[folderID] = struct{}{}

		children, err := session.meta.ListChildren(ctx, parent.remoteDriveID.String(), folderID)
		if err != nil {
			return nil, fmt.Errorf("listing children for parent mount %s folder %s: %w", parent.mountID, folderID, err)
		}

		for i := range children {
			_, knownBinding := existingByBinding[children[i].ID]
			binding, ok, bindErr := resolveDiscoveredShortcutBinding(
				ctx,
				session,
				parent,
				rootPathPrefix,
				&children[i],
				knownBinding,
			)
			if bindErr != nil {
				return nil, bindErr
			}
			if ok {
				bindings = append(bindings, binding)
				continue
			}
			if children[i].IsFolder {
				queue = append(queue, children[i].ID)
			}
		}
	}

	return bindings, nil
}

func resolveDiscoveredShortcutBinding(
	ctx context.Context,
	session *driveopsSessionView,
	parent *mountSpec,
	rootPathPrefix string,
	item *graph.Item,
	knownBinding bool,
) (discoveredShortcutBinding, bool, error) {
	if item == nil || item.IsDeleted || item.ID == "" {
		return discoveredShortcutBinding{}, false, nil
	}

	current := item
	if requiresShortcutBindingRefresh(current) {
		refreshed, err := session.meta.GetItem(ctx, parent.remoteDriveID.String(), current.ID)
		if err != nil {
			if knownBinding || hasShortcutBindingEvidence(current) {
				return unavailableShortcutBinding(rootPathPrefix, current), true, nil
			}
			return discoveredShortcutBinding{}, false, fmt.Errorf(
				"refreshing shortcut placeholder %s for parent mount %s: %w",
				current.ID,
				parent.mountID,
				err,
			)
		}
		current = refreshed
	}

	if !isShortcutPlaceholder(current) {
		return discoveredShortcutBinding{}, false, nil
	}

	relativeLocalPath, err := shortcutRelativeLocalPath(rootPathPrefix, current)
	if err != nil {
		return discoveredShortcutBinding{}, false, fmt.Errorf(
			"materializing shortcut placeholder path for %s under parent mount %s: %w",
			current.ID,
			parent.mountID,
			err,
		)
	}

	return discoveredShortcutBinding{
		BindingItemID:     current.ID,
		LocalAlias:        current.Name,
		RelativeLocalPath: relativeLocalPath,
		RemoteDriveID:     current.RemoteDriveID,
		RemoteItemID:      current.RemoteItemID,
	}, true, nil
}

func unavailableShortcutBinding(rootPathPrefix string, item *graph.Item) discoveredShortcutBinding {
	binding := discoveredShortcutBinding{
		State:       config.MountStateUnavailable,
		StateReason: config.MountStateReasonShortcutBindingUnavailable,
	}
	if item == nil {
		return binding
	}

	binding.BindingItemID = item.ID
	binding.LocalAlias = item.Name
	binding.RemoteDriveID = item.RemoteDriveID
	binding.RemoteItemID = item.RemoteItemID
	if relativeLocalPath, err := shortcutRelativeLocalPath(rootPathPrefix, item); err == nil {
		binding.RelativeLocalPath = relativeLocalPath
	}

	return binding
}

func requiresShortcutBindingRefresh(item *graph.Item) bool {
	if item == nil {
		return false
	}

	return item.Name == "" || item.ParentPath == "" || item.RemoteDriveID == "" || item.RemoteItemID == ""
}

func hasShortcutBindingEvidence(item *graph.Item) bool {
	return item != nil && item.IsFolder && (item.RemoteDriveID != "" || item.RemoteItemID != "")
}

func isShortcutPlaceholder(item *graph.Item) bool {
	if item == nil {
		return false
	}

	return item.IsFolder && item.RemoteDriveID != "" && item.RemoteItemID != ""
}

func shortcutRelativeLocalPath(rootPathPrefix string, item *graph.Item) (string, error) {
	if item == nil || item.Name == "" {
		return "", fmt.Errorf("shortcut placeholder is missing a name")
	}

	fullPath := item.Name
	if item.ParentPath != "" {
		fullPath = path.Join(item.ParentPath, item.Name)
	}
	fullPath = path.Clean(strings.TrimPrefix(fullPath, "/"))
	if fullPath == "." || fullPath == "" {
		return "", fmt.Errorf("shortcut placeholder path is empty")
	}

	if rootPathPrefix == "" {
		return fullPath, nil
	}

	rootPrefix := path.Clean(strings.TrimPrefix(rootPathPrefix, "/"))
	if fullPath == rootPrefix {
		return "", fmt.Errorf("shortcut placeholder path resolves to the mount root")
	}
	if !strings.HasPrefix(fullPath+"/", rootPrefix+"/") {
		return "", fmt.Errorf("shortcut placeholder path %q escapes mount root %q", fullPath, rootPrefix)
	}

	return strings.TrimPrefix(fullPath, rootPrefix+"/"), nil
}

func mountedRootPathPrefix(ctx context.Context, session *driveops.Session, parent *mountSpec) (string, error) {
	if parent == nil || parent.remoteRootItemID == "" {
		return "", nil
	}
	root, err := session.Meta.GetItem(ctx, parent.remoteDriveID, parent.remoteRootItemID)
	if err != nil {
		return "", fmt.Errorf("resolving mount root item %s for parent mount %s: %w", parent.remoteRootItemID, parent.mountID, err)
	}
	if root == nil || root.Name == "" {
		return "", fmt.Errorf("resolving mount root item %s for parent mount %s: missing folder name", parent.remoteRootItemID, parent.mountID)
	}

	if root.ParentPath == "" {
		return root.Name, nil
	}

	return path.Join(root.ParentPath, root.Name), nil
}

func newDriveopsSessionView(session *driveops.Session) *driveopsSessionView {
	if session == nil {
		return nil
	}

	return &driveopsSessionView{
		meta: &graphDiscoveryAdapter{
			client: session.Meta,
		},
	}
}

type graphDiscoveryAdapter struct {
	client *graph.Client
}

func (a *graphDiscoveryAdapter) DeltaAll(ctx context.Context, driveID string, token string) ([]graph.Item, string, error) {
	items, nextToken, err := a.client.DeltaAll(ctx, driveid.New(driveID), token)
	if err != nil {
		return nil, "", fmt.Errorf("delta root for drive %s: %w", driveID, err)
	}

	return items, nextToken, nil
}

func (a *graphDiscoveryAdapter) DeltaFolderAll(ctx context.Context, driveID string, folderID, token string) ([]graph.Item, string, error) {
	items, nextToken, err := a.client.DeltaFolderAll(ctx, driveid.New(driveID), folderID, token)
	if err != nil {
		return nil, "", fmt.Errorf("delta folder %s for drive %s: %w", folderID, driveID, err)
	}

	return items, nextToken, nil
}

func (a *graphDiscoveryAdapter) GetItem(ctx context.Context, driveID string, itemID string) (*graph.Item, error) {
	item, err := a.client.GetItem(ctx, driveid.New(driveID), itemID)
	if err != nil {
		return nil, fmt.Errorf("get item %s for drive %s: %w", itemID, driveID, err)
	}

	return item, nil
}

func (a *graphDiscoveryAdapter) ListChildren(ctx context.Context, driveID string, parentID string) ([]graph.Item, error) {
	items, err := a.client.ListChildren(ctx, driveid.New(driveID), parentID)
	if err != nil {
		return nil, fmt.Errorf("list children for parent %s in drive %s: %w", parentID, driveID, err)
	}

	return items, nil
}

func purgeManagedMountStateDBs(logger *slog.Logger, mountIDs []string) error {
	var errs []error
	for _, mountID := range mountIDs {
		if mountID == "" {
			continue
		}
		if err := syncengine.RemoveStateDBFiles(config.MountStatePath(mountID)); err != nil {
			errs = append(errs, fmt.Errorf("purging removed child mount state %s: %w", mountID, err))
			continue
		}
		if logger != nil {
			logger.Info("purged removed child mount state",
				"mount_id", mountID,
			)
		}
	}

	return errors.Join(errs...)
}

func finalizePendingMountRemovals(mountIDs []string) error {
	if len(mountIDs) == 0 {
		return nil
	}

	if err := config.UpdateMountInventory(func(inventory *config.MountInventory) error {
		for _, mountID := range mountIDs {
			record, found := inventory.Mounts[mountID]
			if !found || record.State != config.MountStatePendingRemoval {
				continue
			}
			delete(inventory.Mounts, mountID)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("updating mount inventory after child mount removal: %w", err)
	}

	return nil
}
