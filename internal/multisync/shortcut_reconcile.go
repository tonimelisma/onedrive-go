package multisync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type reconcileResult struct {
	RemovedMountIDs []string
}

type discoveredShortcutBinding struct {
	BindingItemID     string
	DisplayName       string
	RelativeLocalPath string
	RemoteDriveID     string
	RemoteRootItemID  string
}

const (
	driveRootItemID           = "root"
	initialSeenFolderCapacity = 16
)

type parentReconcileResult struct {
	changed         bool
	removedMountIDs []string
}

func (o *Orchestrator) reconcileManagedShortcutMounts(
	ctx context.Context,
	parents []*mountSpec,
) (reconcileResult, error) {
	if len(parents) == 0 {
		return reconcileResult{}, nil
	}

	inventory, err := config.LoadMountInventory()
	if err != nil {
		return reconcileResult{}, fmt.Errorf("loading mount inventory: %w", err)
	}

	result := reconcileResult{}
	changed := false
	for _, parent := range parents {
		parentResult, parentErr := o.reconcileParentMount(ctx, inventory, parent)
		if parentErr != nil {
			if o.logger != nil {
				o.logger.Warn("shortcut reconciliation failed",
					"parent_mount_id", parent.mountID.String(),
					"error", parentErr,
				)
			}
			continue
		}
		changed = changed || parentResult.changed
		result.RemovedMountIDs = append(result.RemovedMountIDs, parentResult.removedMountIDs...)
	}

	if !changed {
		return result, nil
	}

	if err := config.SaveMountInventory(inventory); err != nil {
		return reconcileResult{}, fmt.Errorf("saving mount inventory: %w", err)
	}

	return result, nil
}

func (o *Orchestrator) reconcileParentMount(
	ctx context.Context,
	inventory *config.MountInventory,
	parent *mountSpec,
) (parentReconcileResult, error) {
	if inventory == nil || parent == nil {
		return parentReconcileResult{}, nil
	}

	session, err := o.cfg.Runtime.SyncSession(ctx, parent.syncSessionConfig())
	if err != nil {
		return parentReconcileResult{}, fmt.Errorf("session for parent mount %s: %w", parent.mountID, err)
	}
	sessionView := newDriveopsSessionView(session)

	rootPathPrefix, err := mountedRootPathPrefix(ctx, session, parent)
	if err != nil {
		return parentReconcileResult{}, err
	}

	state := inventory.Parents[parent.mountID.String()]
	if state.ParentMountID == "" {
		state.ParentMountID = parent.mountID.String()
	}

	switch {
	case parent.remoteRootItemID == "":
		return o.reconcileParentMountDelta(ctx, inventory, parent, sessionView, rootPathPrefix, state, false)
	case parent.rootedSubtreeDeltaCapable:
		result, deltaErr := o.reconcileParentMountDelta(ctx, inventory, parent, sessionView, rootPathPrefix, state, true)
		if deltaErr == nil {
			return result, nil
		}
		if errors.Is(deltaErr, graph.ErrMethodNotAllowed) || errors.Is(deltaErr, graph.ErrNotFound) {
			return o.reconcileParentMountByListing(ctx, inventory, parent, sessionView, rootPathPrefix, state)
		}
		return parentReconcileResult{}, deltaErr
	default:
		return o.reconcileParentMountByListing(ctx, inventory, parent, sessionView, rootPathPrefix, state)
	}
}

func (o *Orchestrator) reconcileParentMountDelta(
	ctx context.Context,
	inventory *config.MountInventory,
	parent *mountSpec,
	session *driveopsSessionView,
	rootPathPrefix string,
	state config.ParentDiscoveryState,
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
			inventory.Parents[parent.mountID.String()] = state
			return parentReconcileResult{changed: changed}, nil
		}
		return parentReconcileResult{}, fmt.Errorf("delta discovery for parent mount %s: %w", parent.mountID, err)
	}

	state.DeltaLink = newToken
	state.DiscoveryMode = config.DiscoveryModeDelta
	inventory.Parents[parent.mountID.String()] = state

	existingByBinding := existingBindingsForParent(inventory, parent.mountID)
	if fullEnumeration {
		discovered := make(map[string]discoveredShortcutBinding)
		for i := range items {
			binding, ok, bindErr := resolveDiscoveredShortcutBinding(ctx, session, parent, rootPathPrefix, &items[i])
			if bindErr != nil {
				return parentReconcileResult{}, bindErr
			}
			if !ok {
				continue
			}
			discovered[binding.BindingItemID] = binding
		}
		return applyFullParentEnumeration(inventory, parent.mountID, existingByBinding, discovered)
	}

	return applyIncrementalParentDelta(ctx, inventory, parent.mountID, existingByBinding, session, parent, rootPathPrefix, items)
}

func (o *Orchestrator) reconcileParentMountByListing(
	ctx context.Context,
	inventory *config.MountInventory,
	parent *mountSpec,
	session *driveopsSessionView,
	rootPathPrefix string,
	state config.ParentDiscoveryState,
) (parentReconcileResult, error) {
	bindings, err := enumerateShortcutBindings(ctx, session, parent, rootPathPrefix)
	if err != nil {
		return parentReconcileResult{}, err
	}

	state.ParentMountID = parent.mountID.String()
	state.DeltaLink = ""
	state.DiscoveryMode = config.DiscoveryModeEnumerate
	inventory.Parents[parent.mountID.String()] = state

	changed := false
	existingByBinding := existingBindingsForParent(inventory, parent.mountID)
	for _, binding := range bindings {
		updated, err := upsertChildMountRecord(inventory, parent.mountID, existingByBinding, binding)
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

func existingBindingsForParent(inventory *config.MountInventory, parentID mountID) map[string]config.MountRecord {
	byBinding := make(map[string]config.MountRecord)
	for _, record := range inventory.Mounts {
		if record.ParentMountID != parentID.String() || record.BindingItemID == "" {
			continue
		}
		byBinding[record.BindingItemID] = record
	}
	return byBinding
}

func applyFullParentEnumeration(
	inventory *config.MountInventory,
	parentID mountID,
	existingByBinding map[string]config.MountRecord,
	discovered map[string]discoveredShortcutBinding,
) (parentReconcileResult, error) {
	changed := false
	removed := make([]string, 0)
	for bindingID, binding := range discovered {
		updated, err := upsertChildMountRecord(inventory, parentID, existingByBinding, binding)
		if err != nil {
			return parentReconcileResult{}, err
		}
		changed = changed || updated
		delete(existingByBinding, bindingID)
	}

	for bindingID, record := range existingByBinding {
		delete(inventory.Mounts, record.MountID)
		delete(existingByBinding, bindingID)
		removed = append(removed, record.MountID)
		changed = true
	}

	return parentReconcileResult{changed: changed, removedMountIDs: removed}, nil
}

func applyIncrementalParentDelta(
	ctx context.Context,
	inventory *config.MountInventory,
	parentID mountID,
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
				delete(inventory.Mounts, record.MountID)
				removed = append(removed, record.MountID)
				changed = true
			}
			continue
		}

		binding, ok, err := resolveDiscoveredShortcutBinding(ctx, session, parent, rootPathPrefix, item)
		if err != nil {
			return parentReconcileResult{}, err
		}
		if !ok {
			if found {
				delete(inventory.Mounts, record.MountID)
				removed = append(removed, record.MountID)
				changed = true
			}
			continue
		}

		updated, err := upsertChildMountRecord(inventory, parentID, existingByBinding, binding)
		if err != nil {
			return parentReconcileResult{}, err
		}
		changed = changed || updated
	}

	return parentReconcileResult{changed: changed, removedMountIDs: removed}, nil
}

func upsertChildMountRecord(
	inventory *config.MountInventory,
	parentID mountID,
	existingByBinding map[string]config.MountRecord,
	binding discoveredShortcutBinding,
) (bool, error) {
	record, found := existingByBinding[binding.BindingItemID]
	if !found {
		record = config.MountRecord{
			MountID:       config.ChildMountID(parentID.String(), binding.BindingItemID),
			ParentMountID: parentID.String(),
			BindingItemID: binding.BindingItemID,
		}
	}

	if record.MountID == "" {
		return false, fmt.Errorf("constructing child mount ID for parent %s binding %s", parentID, binding.BindingItemID)
	}

	next := record
	next.ParentMountID = parentID.String()
	next.BindingItemID = binding.BindingItemID
	next.DisplayName = binding.DisplayName
	next.RelativeLocalPath = binding.RelativeLocalPath
	next.RemoteDriveID = binding.RemoteDriveID
	next.RemoteRootItemID = binding.RemoteRootItemID

	if found && next == record {
		return false, nil
	}

	if found && record.MountID != next.MountID {
		delete(inventory.Mounts, record.MountID)
	}
	inventory.Mounts[next.MountID] = next
	existingByBinding[binding.BindingItemID] = next
	return true, nil
}

func enumerateShortcutBindings(
	ctx context.Context,
	session *driveopsSessionView,
	parent *mountSpec,
	rootPathPrefix string,
) ([]discoveredShortcutBinding, error) {
	rootItemID := parent.remoteRootItemID
	if rootItemID == "" {
		rootItemID = driveRootItemID
	}

	queue := []string{rootItemID}
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
			binding, ok, bindErr := resolveDiscoveredShortcutBinding(ctx, session, parent, rootPathPrefix, &children[i])
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
) (discoveredShortcutBinding, bool, error) {
	if item == nil || item.IsDeleted || item.ID == "" {
		return discoveredShortcutBinding{}, false, nil
	}

	current := item
	if requiresShortcutBindingRefresh(current) {
		refreshed, err := session.meta.GetItem(ctx, parent.remoteDriveID.String(), current.ID)
		if err != nil {
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
		DisplayName:       current.Name,
		RelativeLocalPath: relativeLocalPath,
		RemoteDriveID:     current.RemoteDriveID,
		RemoteRootItemID:  current.RemoteItemID,
	}, true, nil
}

func requiresShortcutBindingRefresh(item *graph.Item) bool {
	if item == nil {
		return false
	}

	return item.Name == "" || item.ParentPath == "" || item.RemoteDriveID == "" || item.RemoteItemID == ""
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
		return "", fmt.Errorf("shortcut placeholder path %q escapes mounted root %q", fullPath, rootPrefix)
	}

	return strings.TrimPrefix(fullPath, rootPrefix+"/"), nil
}

func mountedRootPathPrefix(ctx context.Context, session *driveops.Session, parent *mountSpec) (string, error) {
	if parent == nil || parent.remoteRootItemID == "" {
		return "", nil
	}
	root, err := session.Meta.GetItem(ctx, parent.remoteDriveID, parent.remoteRootItemID)
	if err != nil {
		return "", fmt.Errorf("resolving mounted root item %s for parent mount %s: %w", parent.remoteRootItemID, parent.mountID, err)
	}
	if root == nil || root.Name == "" {
		return "", fmt.Errorf("resolving mounted root item %s for parent mount %s: missing folder name", parent.remoteRootItemID, parent.mountID)
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
