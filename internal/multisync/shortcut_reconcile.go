package multisync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/graph"
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
	dirtyMountIDs   []string
}

type namespaceReconcileResult struct {
	inventory       *config.MountInventory
	removedMountIDs []string
	dirtyMountIDs   []string
	persistErr      error
}

type mountInventoryMutationResult struct {
	changed          bool
	dirtyMountIDs    []string
	localRootActions []childRootLifecycleAction
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
		result.dirtyMountIDs = appendUniqueStrings(result.dirtyMountIDs, parentResult.dirtyMountIDs...)
	}
	conflictResult := applyDurableProjectionConflicts(inventory, parents)
	changed = conflictResult.changed || changed
	result.dirtyMountIDs = appendUniqueStrings(result.dirtyMountIDs, conflictResult.dirtyMountIDs...)
	result.removedMountIDs = appendUniqueStrings(result.removedMountIDs, pendingRemovalMountIDs(inventory)...)

	if !changed {
		return result, nil
	}

	if err := config.SaveMountInventory(inventory); err != nil {
		result.persistErr = fmt.Errorf("saving mount inventory: %w", err)
		if n.logger != nil {
			n.logger.Warn("shortcut reconciliation inventory was not persisted; continuing with in-memory mount inventory",
				slog.String("error", result.persistErr.Error()),
			)
		}
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

	var result parentReconcileResult
	switch {
	case parent.remoteRootItemID == "":
		result, err = n.reconcileNamespaceMountDelta(ctx, inventory, parent, sessionView, rootPathPrefix, state, false)
	case parent.remoteRootDeltaCapable:
		var deltaErr error
		result, deltaErr = n.reconcileNamespaceMountDelta(ctx, inventory, parent, sessionView, rootPathPrefix, state, true)
		if deltaErr == nil {
			retryResult := n.retryUnavailableShortcutBindings(ctx, inventory, parent, sessionView, rootPathPrefix)
			return mergeParentReconcileResults(result, retryResult), nil
		}
		if errors.Is(deltaErr, graph.ErrMethodNotAllowed) || errors.Is(deltaErr, graph.ErrNotFound) {
			result, err = n.reconcileNamespaceMountByListing(ctx, inventory, parent, sessionView, rootPathPrefix, state)
			break
		}
		return parentReconcileResult{}, deltaErr
	default:
		result, err = n.reconcileNamespaceMountByListing(ctx, inventory, parent, sessionView, rootPathPrefix, state)
	}
	if err != nil {
		return parentReconcileResult{}, err
	}

	retryResult := n.retryUnavailableShortcutBindings(ctx, inventory, parent, sessionView, rootPathPrefix)
	return mergeParentReconcileResults(result, retryResult), nil
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
	dirtyMountIDs := make([]string, 0)
	for _, binding := range bindings {
		updated, dirtyMountID, err := upsertChildMountRecord(inventory, parent, existingByBinding, binding)
		if err != nil {
			return parentReconcileResult{}, err
		}
		changed = changed || updated
		if updated {
			dirtyMountIDs = appendUniqueStrings(dirtyMountIDs, dirtyMountID)
		}
	}

	return parentReconcileResult{changed: changed, dirtyMountIDs: dirtyMountIDs}, nil
}

func (n *namespaceRuntime) retryUnavailableShortcutBindings(
	ctx context.Context,
	inventory *config.MountInventory,
	parent *mountSpec,
	session *driveopsSessionView,
	rootPathPrefix string,
) parentReconcileResult {
	if inventory == nil || parent == nil || session == nil {
		return parentReconcileResult{}
	}

	existingByBinding := existingBindingsForNamespace(inventory, parent.mountID)
	records := sortedMountRecords(inventory)
	result := parentReconcileResult{}
	for i := range records {
		record := &records[i]
		if !shouldRetryUnavailableShortcutBinding(record, parent.mountID) {
			continue
		}

		binding, ok := n.refreshUnavailableShortcutBinding(ctx, session, parent, rootPathPrefix, record)
		if !ok {
			continue
		}

		updated, dirtyMountID, updateErr := upsertChildMountRecord(inventory, parent, existingByBinding, binding)
		if updateErr != nil {
			if n.logger != nil {
				n.logger.Warn("reactivating unavailable shortcut binding failed",
					slog.String("mount_id", record.MountID),
					slog.String("binding_item_id", record.BindingItemID),
					slog.String("error", updateErr.Error()),
				)
			}
			continue
		}
		if updated {
			result.changed = true
			result.dirtyMountIDs = appendUniqueStrings(result.dirtyMountIDs, dirtyMountID)
		}
	}

	return result
}

func shouldRetryUnavailableShortcutBinding(record *config.MountRecord, parentID mountID) bool {
	return record != nil &&
		record.NamespaceID == parentID.String() &&
		record.BindingItemID != "" &&
		record.State == config.MountStateUnavailable &&
		record.StateReason == config.MountStateReasonShortcutBindingUnavailable
}

func (n *namespaceRuntime) refreshUnavailableShortcutBinding(
	ctx context.Context,
	session *driveopsSessionView,
	parent *mountSpec,
	rootPathPrefix string,
	record *config.MountRecord,
) (discoveredShortcutBinding, bool) {
	refreshed, err := session.meta.GetItem(ctx, parent.remoteDriveID.String(), record.BindingItemID)
	if err != nil {
		if n.logger != nil {
			n.logger.Debug("shortcut binding remains unavailable",
				slog.String("mount_id", record.MountID),
				slog.String("binding_item_id", record.BindingItemID),
				slog.String("error", err.Error()),
			)
		}
		return discoveredShortcutBinding{}, false
	}

	binding, ok, bindErr := resolveDiscoveredShortcutBinding(
		ctx,
		session,
		parent,
		rootPathPrefix,
		refreshed,
		true,
	)
	if bindErr != nil {
		if n.logger != nil {
			n.logger.Warn("refreshing unavailable shortcut binding failed",
				slog.String("mount_id", record.MountID),
				slog.String("binding_item_id", record.BindingItemID),
				slog.String("error", bindErr.Error()),
			)
		}
		return discoveredShortcutBinding{}, false
	}
	if !ok || binding.State == config.MountStateUnavailable {
		return discoveredShortcutBinding{}, false
	}

	return binding, true
}

func mergeParentReconcileResults(results ...parentReconcileResult) parentReconcileResult {
	merged := parentReconcileResult{}
	for i := range results {
		merged.changed = merged.changed || results[i].changed
		merged.removedMountIDs = appendUniqueStrings(merged.removedMountIDs, results[i].removedMountIDs...)
		merged.dirtyMountIDs = appendUniqueStrings(merged.dirtyMountIDs, results[i].dirtyMountIDs...)
	}

	return merged
}
