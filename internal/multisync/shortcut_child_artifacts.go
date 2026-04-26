package multisync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type releasedShortcutChild struct {
	mountID       string
	namespaceID   string
	bindingItemID string
	reason        syncengine.ShortcutChildReleaseReason
}

type shortcutChildArtifactScope struct {
	mountID   string
	localRoot string
}

func releasedChildren(
	topologies map[mountID]syncengine.ShortcutChildTopologyPublication,
) []releasedShortcutChild {
	if len(topologies) == 0 {
		return nil
	}

	releases := make([]releasedShortcutChild, 0)
	for parentID, publication := range topologies {
		namespaceID := parentID.String()
		if publication.NamespaceID != "" {
			namespaceID = publication.NamespaceID
		}
		for i := range publication.Released {
			release := publication.Released[i]
			if release.BindingItemID == "" {
				continue
			}
			reason := release.Reason
			if reason == "" {
				reason = syncengine.ShortcutChildReleaseParentRemoved
			}
			releases = append(releases, releasedShortcutChild{
				mountID:       config.ChildMountID(namespaceID, release.BindingItemID),
				namespaceID:   namespaceID,
				bindingItemID: release.BindingItemID,
				reason:        reason,
			})
		}
	}
	sort.Slice(releases, func(i, j int) bool {
		if releases[i].mountID == releases[j].mountID {
			return releases[i].bindingItemID < releases[j].bindingItemID
		}
		return releases[i].mountID < releases[j].mountID
	})
	return releases
}

func (o *Orchestrator) purgeReleasedShortcutChildArtifactsForCompiled(
	ctx context.Context,
	compiled *compiledMountSet,
) error {
	if compiled == nil || len(compiled.ReleasedChildren) == 0 {
		return nil
	}
	var errs []error
	purged := make([]releasedShortcutChild, 0, len(compiled.ReleasedChildren))
	for _, release := range compiled.ReleasedChildren {
		scope := shortcutChildArtifactScope{mountID: release.mountID}
		if err := purgeShortcutChildArtifacts(ctx, scope, o.logger); err != nil {
			errs = append(errs, fmt.Errorf("purging released child mount %s: %w", release.mountID, err))
			continue
		}
		purged = append(purged, release)
	}
	if len(purged) > 0 {
		o.forgetReleasedShortcutChildren(purged)
	}
	return errors.Join(errs...)
}

func purgeShortcutChildArtifacts(ctx context.Context, scope shortcutChildArtifactScope, logger *slog.Logger) error {
	childMountID := strings.TrimSpace(scope.mountID)
	if strings.TrimSpace(childMountID) == "" {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return fmt.Errorf("purging shortcut child artifacts: %w", ctx.Err())
	}
	if !config.IsChildMountID(childMountID) {
		return nil
	}

	var errs []error
	for _, path := range shortcutChildStateArtifactPaths(childMountID) {
		if path == "" {
			continue
		}
		if err := localpath.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove child state artifact %s: %w", path, err))
		}
	}
	if err := pruneShortcutChildCatalogRecord(childMountID); err != nil {
		errs = append(errs, err)
	}
	sessionStore := driveops.NewSessionStore(config.DefaultDataDir(), logger)
	if _, err := sessionStore.DeleteForScope(childMountID, scope.localRoot); err != nil {
		errs = append(errs, fmt.Errorf("delete child upload sessions: %w", err))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("purging managed child mount artifacts: %w", err)
	}

	if logger != nil {
		logger.Info("purged shortcut child state artifacts",
			slog.String("mount_id", childMountID),
		)
	}
	return nil
}

func shortcutChildStateArtifactPaths(childMountID string) []string {
	statePath := config.MountStatePath(childMountID)
	if statePath == "" {
		return nil
	}
	return []string{
		statePath,
		statePath + "-wal",
		statePath + "-shm",
		statePath + "-journal",
	}
}

func pruneShortcutChildCatalogRecord(childMountID string) error {
	if !config.IsChildMountID(childMountID) {
		return nil
	}
	catalogPath := config.CatalogPath()
	if catalogPath == "" {
		return nil
	}
	if _, err := localpath.Stat(catalogPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat catalog before child cleanup: %w", err)
	}

	catalog, err := config.LoadCatalog()
	if err != nil {
		return fmt.Errorf("load catalog before child cleanup: %w", err)
	}
	if _, found := catalog.Drives[childMountID]; !found {
		return nil
	}

	if err := config.UpdateCatalog(func(catalog *config.Catalog) error {
		delete(catalog.Drives, childMountID)
		return nil
	}); err != nil {
		return fmt.Errorf("update catalog for child cleanup: %w", err)
	}
	return nil
}
