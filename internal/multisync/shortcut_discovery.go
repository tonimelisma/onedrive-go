package multisync

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

type driveopsSessionView struct {
	meta graphDiscoveryClient
}

type graphDiscoveryClient interface {
	DeltaAll(ctx context.Context, driveID string, token string) ([]graph.Item, string, error)
	DeltaFolderAll(ctx context.Context, driveID string, folderID, token string) ([]graph.Item, string, error)
	GetItem(ctx context.Context, driveID string, itemID string) (*graph.Item, error)
	ListChildren(ctx context.Context, driveID string, parentID string) ([]graph.Item, error)
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

	needsRemoteKind := item.RemoteDriveID != "" && item.RemoteItemID != "" && !item.IsFolder && !item.RemoteIsFolder

	return item.Name == "" || item.ParentPath == "" || item.RemoteDriveID == "" || item.RemoteItemID == "" || needsRemoteKind
}

func hasShortcutBindingEvidence(item *graph.Item) bool {
	return item != nil && (item.IsFolder || item.RemoteIsFolder) && (item.RemoteDriveID != "" || item.RemoteItemID != "")
}

func isShortcutPlaceholder(item *graph.Item) bool {
	if item == nil {
		return false
	}

	return (item.RemoteIsFolder || item.IsFolder) && item.RemoteDriveID != "" && item.RemoteItemID != ""
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
