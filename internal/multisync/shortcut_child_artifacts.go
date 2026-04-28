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
	localRoot     string
	reason        syncengine.ShortcutChildArtifactCleanupReason
}

type shortcutChildArtifactCleanupSource string

const (
	shortcutChildArtifactCleanupSourcePublished  shortcutChildArtifactCleanupSource = "published_cleanup"
	shortcutChildArtifactCleanupSourceFinalDrain shortcutChildArtifactCleanupSource = "final_drain"
)

type shortcutChildArtifactCleanupFailureClass string

const (
	shortcutChildArtifactCleanupSetupFailure         shortcutChildArtifactCleanupFailureClass = "setup"
	shortcutChildArtifactCleanupStateArtifactFailure shortcutChildArtifactCleanupFailureClass = "state_artifact"
	shortcutChildArtifactCleanupCatalogFailure       shortcutChildArtifactCleanupFailureClass = "catalog"
	shortcutChildArtifactCleanupUploadSessionFailure shortcutChildArtifactCleanupFailureClass = "upload_session"
	shortcutChildArtifactCleanupParentAckFailure     shortcutChildArtifactCleanupFailureClass = "parent_ack"
)

type shortcutChildArtifactCleanupError struct {
	class shortcutChildArtifactCleanupFailureClass
	op    string
	err   error
}

func (e shortcutChildArtifactCleanupError) Error() string {
	if e.op == "" {
		return fmt.Sprintf("%s cleanup failure: %v", e.class, e.err)
	}
	return fmt.Sprintf("%s cleanup failure: %s: %v", e.class, e.op, e.err)
}

func (e shortcutChildArtifactCleanupError) Unwrap() error {
	return e.err
}

func shortcutChildArtifactCleanupClass(err error) (shortcutChildArtifactCleanupFailureClass, bool) {
	var cleanupErr shortcutChildArtifactCleanupError
	if errors.As(err, &cleanupErr) {
		return cleanupErr.class, true
	}
	return "", false
}

func classifiedShortcutChildCleanupError(
	class shortcutChildArtifactCleanupFailureClass,
	op string,
	err error,
) error {
	if err == nil {
		return nil
	}
	return shortcutChildArtifactCleanupError{
		class: class,
		op:    op,
		err:   err,
	}
}

type shortcutChildArtifactScope struct {
	mountID   string
	localRoot string
}

type shortcutChildArtifactCleanupExecutor struct {
	dataDir              string
	logger               *slog.Logger
	remove               func(string) error
	pruneCatalogRecord   func(string) error
	deleteUploadSessions func(string, string) error
}

func newShortcutChildArtifactCleanupExecutor(logger *slog.Logger, dataDir string) shortcutChildArtifactCleanupExecutor {
	return shortcutChildArtifactCleanupExecutor{
		dataDir: dataDir,
		logger:  logger,
		remove:  localpath.Remove,
		pruneCatalogRecord: func(childMountID string) error {
			return pruneShortcutChildCatalogRecord(dataDir, childMountID)
		},
		deleteUploadSessions: func(childMountID string, localRoot string) error {
			sessionStore := driveops.NewSessionStore(dataDir, logger)
			_, err := sessionStore.DeleteForScope(childMountID, localRoot)
			if err != nil {
				return fmt.Errorf("delete upload sessions for child mount %s: %w", childMountID, err)
			}
			return nil
		},
	}
}

func (o *Orchestrator) childArtifactCleanupExecutor() shortcutChildArtifactCleanupExecutor {
	if o == nil || o.artifactCleanup.remove == nil {
		return shortcutChildArtifactCleanupExecutor{}
	}
	return o.artifactCleanup
}

func shortcutChildArtifactCleanups(
	publications map[mountID]syncengine.ShortcutChildRunnerPublication,
) ([]shortcutChildArtifactCleanup, error) {
	if len(publications) == 0 {
		return nil, nil
	}

	cleanups := make([]shortcutChildArtifactCleanup, 0)
	for parentID, publication := range publications {
		namespaceID := parentID.String()
		if publication.NamespaceID != "" {
			namespaceID = publication.NamespaceID
		}
		for i := range publication.CleanupWork.Requests {
			request := publication.CleanupWork.Requests[i]
			if request.BindingItemID == "" {
				return nil, fmt.Errorf("parent cleanup request from %s is missing binding item ID", namespaceID)
			}
			reason := request.Reason
			if reason == "" {
				return nil, fmt.Errorf("parent cleanup request for binding %s is missing cleanup reason", request.BindingItemID)
			}
			childMountID := request.ChildMountID
			if childMountID == "" {
				return nil, fmt.Errorf("parent cleanup request for binding %s is missing child mount ID", request.BindingItemID)
			}
			if request.LocalRoot == "" {
				return nil, fmt.Errorf("parent cleanup request for binding %s is missing child local root", request.BindingItemID)
			}
			cleanups = append(cleanups, shortcutChildArtifactCleanup{
				mountID:       childMountID,
				namespaceID:   namespaceID,
				bindingItemID: request.BindingItemID,
				localRoot:     request.LocalRoot,
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
	return cleanups, nil
}

func (o *Orchestrator) purgeShortcutChildArtifactsForDecisions(
	ctx context.Context,
	decisions *runnerDecisionSet,
	parentAckers map[mountID]shortcutChildAckHandle,
) error {
	if decisions == nil || len(decisions.CleanupChildren) == 0 {
		return nil
	}
	return o.purgeAndAcknowledgeShortcutChildArtifacts(
		ctx,
		shortcutChildArtifactCleanupSourcePublished,
		decisions.CleanupChildren,
		parentAckers,
	)
}

func (o *Orchestrator) purgeAndAcknowledgeShortcutChildArtifacts(
	ctx context.Context,
	source shortcutChildArtifactCleanupSource,
	cleanups []shortcutChildArtifactCleanup,
	parentAckers map[mountID]shortcutChildAckHandle,
) error {
	var errs []error
	purged := make([]shortcutChildArtifactCleanup, 0, len(cleanups))
	for _, cleanup := range cleanups {
		scope := shortcutChildArtifactScope{mountID: cleanup.mountID, localRoot: cleanup.localRoot}
		if err := o.childArtifactCleanupExecutor().purge(ctx, scope); err != nil {
			errs = append(errs, fmt.Errorf("purging %s shortcut child mount %s: %w", source, cleanup.mountID, err))
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
	parentAckers map[mountID]shortcutChildAckHandle,
) error {
	var errs []error
	for _, cleanup := range cleanups {
		parentID := mountID(cleanup.namespaceID)
		acker := parentAckers[parentID]
		if shortcutChildAckHandleIsZero(acker) {
			errs = append(errs, classifiedShortcutChildCleanupError(
				shortcutChildArtifactCleanupParentAckFailure,
				"acknowledge parent cleanup",
				fmt.Errorf("parent mount %s is unavailable for child artifact cleanup acknowledgement", parentID),
			))
			continue
		}
		snapshot, err := acker.AcknowledgeChildArtifactsPurged(ctx, syncengine.ShortcutChildArtifactCleanupAck{
			BindingItemID: cleanup.bindingItemID,
		})
		if err != nil {
			errs = append(errs, classifiedShortcutChildCleanupError(
				shortcutChildArtifactCleanupParentAckFailure,
				fmt.Sprintf("acknowledge child artifact cleanup for mount %s", cleanup.mountID),
				err,
			))
			continue
		}
		o.receiveParentRunnerPublication(parentID, snapshot)
	}
	return errors.Join(errs...)
}

func (e shortcutChildArtifactCleanupExecutor) purge(
	ctx context.Context,
	scope shortcutChildArtifactScope,
) error {
	childMountID := strings.TrimSpace(scope.mountID)
	if strings.TrimSpace(childMountID) == "" {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return classifiedShortcutChildCleanupError(
			shortcutChildArtifactCleanupSetupFailure,
			"purge shortcut child artifacts",
			ctx.Err(),
		)
	}
	if !config.IsChildMountID(childMountID) {
		return nil
	}
	if err := e.requireConfigured(); err != nil {
		return err
	}

	var errs []error
	for _, path := range shortcutChildStateArtifactPaths(e.dataDir, childMountID) {
		if path == "" {
			continue
		}
		if err := e.remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, classifiedShortcutChildCleanupError(
				shortcutChildArtifactCleanupStateArtifactFailure,
				fmt.Sprintf("remove child state artifact %s", path),
				err,
			))
		}
	}
	if err := e.pruneCatalogRecord(childMountID); err != nil {
		errs = append(errs, classifiedShortcutChildCleanupError(
			shortcutChildArtifactCleanupCatalogFailure,
			"prune child catalog record",
			err,
		))
	}
	if err := e.deleteUploadSessions(childMountID, scope.localRoot); err != nil {
		errs = append(errs, classifiedShortcutChildCleanupError(
			shortcutChildArtifactCleanupUploadSessionFailure,
			"delete child upload sessions",
			err,
		))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("purging managed child mount artifacts: %w", err)
	}

	if e.logger != nil {
		e.logger.Info("purged shortcut child state artifacts",
			slog.String("mount_id", childMountID),
		)
	}
	return nil
}

func (e shortcutChildArtifactCleanupExecutor) requireConfigured() error {
	if strings.TrimSpace(e.dataDir) == "" {
		return classifiedShortcutChildCleanupError(
			shortcutChildArtifactCleanupSetupFailure,
			"configure cleanup executor",
			fmt.Errorf("cleanup executor data dir is not configured"),
		)
	}
	if e.remove == nil || e.pruneCatalogRecord == nil || e.deleteUploadSessions == nil {
		return classifiedShortcutChildCleanupError(
			shortcutChildArtifactCleanupSetupFailure,
			"configure cleanup executor",
			fmt.Errorf("cleanup executor is not configured"),
		)
	}
	return nil
}

func shortcutChildStateArtifactPaths(dataDir string, childMountID string) []string {
	statePath := config.MountStatePathForDataDir(dataDir, childMountID)
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

func pruneShortcutChildCatalogRecord(dataDir string, childMountID string) error {
	if !config.IsChildMountID(childMountID) {
		return nil
	}
	catalogPath := config.CatalogPathForDataDir(dataDir)
	if catalogPath == "" {
		return nil
	}
	if _, err := localpath.Stat(catalogPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat catalog before child cleanup: %w", err)
	}

	catalog, err := config.LoadCatalogForDataDir(dataDir)
	if err != nil {
		return fmt.Errorf("load catalog before child cleanup: %w", err)
	}
	if _, found := catalog.Drives[childMountID]; !found {
		return nil
	}

	if err := config.UpdateCatalogForDataDir(dataDir, func(catalog *config.Catalog) error {
		delete(catalog.Drives, childMountID)
		return nil
	}); err != nil {
		return fmt.Errorf("update catalog for child cleanup: %w", err)
	}
	return nil
}
