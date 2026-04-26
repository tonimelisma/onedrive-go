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

type shortcutChildArtifactCleanup struct {
	mountID       string
	namespaceID   string
	bindingItemID string
	reason        syncengine.ShortcutChildArtifactCleanupReason
}

type shortcutChildArtifactScope struct {
	mountID   string
	localRoot string
}

func shortcutChildArtifactCleanups(
	topologies map[mountID]syncengine.ShortcutChildTopologyPublication,
) []shortcutChildArtifactCleanup {
	if len(topologies) == 0 {
		return nil
	}

	cleanups := make([]shortcutChildArtifactCleanup, 0)
	for parentID, publication := range topologies {
		namespaceID := parentID.String()
		if publication.NamespaceID != "" {
			namespaceID = publication.NamespaceID
		}
		for i := range publication.CleanupRequests {
			request := publication.CleanupRequests[i]
			if request.BindingItemID == "" {
				continue
			}
			reason := request.Reason
			if reason == "" {
				reason = syncengine.ShortcutChildArtifactCleanupParentRemoved
			}
			cleanups = append(cleanups, shortcutChildArtifactCleanup{
				mountID:       config.ChildMountID(namespaceID, request.BindingItemID),
				namespaceID:   namespaceID,
				bindingItemID: request.BindingItemID,
				reason:        reason,
			})
		}
	}
	sort.Slice(cleanups, func(i, j int) bool {
		if cleanups[i].mountID == cleanups[j].mountID {
			return cleanups[i].bindingItemID < cleanups[j].bindingItemID
		}
		return cleanups[i].mountID < cleanups[j].mountID
	})
	return cleanups
}

func (o *Orchestrator) purgeShortcutChildArtifactsForCompiled(
	ctx context.Context,
	compiled *compiledMountSet,
	parentAckers map[mountID]shortcutChildArtifactCleanupAcker,
) error {
	if compiled == nil || len(compiled.CleanupChildren) == 0 {
		return nil
	}
	var errs []error
	purged := make([]shortcutChildArtifactCleanup, 0, len(compiled.CleanupChildren))
	for _, cleanup := range compiled.CleanupChildren {
		scope := shortcutChildArtifactScope{mountID: cleanup.mountID}
		if err := purgeShortcutChildArtifacts(ctx, scope, o.logger); err != nil {
			errs = append(errs, fmt.Errorf("purging shortcut child mount %s: %w", cleanup.mountID, err))
			continue
		}
		purged = append(purged, cleanup)
	}
	if len(purged) > 0 {
		if err := o.acknowledgeShortcutChildArtifactCleanups(ctx, purged, parentAckers); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (o *Orchestrator) acknowledgeShortcutChildArtifactCleanups(
	ctx context.Context,
	cleanups []shortcutChildArtifactCleanup,
	parentAckers map[mountID]shortcutChildArtifactCleanupAcker,
) error {
	var errs []error
	for _, cleanup := range cleanups {
		parentID := mountID(cleanup.namespaceID)
		acker := parentAckers[parentID]
		if acker == nil {
			errs = append(errs, fmt.Errorf("parent mount %s is unavailable for child artifact cleanup acknowledgement", parentID))
			continue
		}
		snapshot, err := acker.AcknowledgeChildArtifactsPurged(ctx, syncengine.ShortcutChildArtifactCleanupAck{
			BindingItemID: cleanup.bindingItemID,
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("acknowledging child artifact cleanup for mount %s: %w", cleanup.mountID, err))
			continue
		}
		o.storeParentShortcutTopology(parentID, snapshot)
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
