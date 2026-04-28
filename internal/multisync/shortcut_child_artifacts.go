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
	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"
)

type shortcutChildArtifactCleanup struct {
	mountID     string
	namespaceID string
	ackRef      syncengine.ShortcutChildAckRef
	localRoot   string
	reason      syncengine.ShortcutChildArtifactCleanupReason
}

type shortcutChildArtifactCleanupSource string

const (
	shortcutChildArtifactCleanupSourcePublished  shortcutChildArtifactCleanupSource = "published_cleanup"
	shortcutChildArtifactCleanupSourceFinalDrain shortcutChildArtifactCleanupSource = "final_drain"
)

type shortcutChildArtifactCleanupFailureClass string

const (
	shortcutChildArtifactCleanupSetupFailure           shortcutChildArtifactCleanupFailureClass = "setup"
	shortcutChildArtifactCleanupStateArtifactFailure   shortcutChildArtifactCleanupFailureClass = "state_artifact"
	shortcutChildArtifactCleanupCatalogFailure         shortcutChildArtifactCleanupFailureClass = "catalog"
	shortcutChildArtifactCleanupTransferScratchFailure shortcutChildArtifactCleanupFailureClass = "transfer_scratch"
	shortcutChildArtifactCleanupUploadSessionFailure   shortcutChildArtifactCleanupFailureClass = "upload_session"
	shortcutChildArtifactCleanupParentAckFailure       shortcutChildArtifactCleanupFailureClass = "parent_ack"
)

const (
	shortcutCleanupDiagnosticArtifactsRemaining = "artifacts_remaining"
	shortcutCleanupDiagnosticParentAckFailed    = "parent_ack_failed"
	shortcutCleanupDiagnosticLimit              = 20
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

func shortcutChildArtifactCleanupClasses(err error) []shortcutChildArtifactCleanupFailureClass {
	if err == nil {
		return nil
	}
	var classes []shortcutChildArtifactCleanupFailureClass
	seen := make(map[shortcutChildArtifactCleanupFailureClass]struct{})
	var walk func(error)
	walk = func(current error) {
		if current == nil {
			return
		}
		if typed, ok := any(current).(shortcutChildArtifactCleanupError); ok {
			if _, ok := seen[typed.class]; !ok {
				seen[typed.class] = struct{}{}
				classes = append(classes, typed.class)
			}
			return
		}
		if typed, ok := any(current).(*shortcutChildArtifactCleanupError); ok {
			if typed != nil {
				if _, alreadySeen := seen[typed.class]; !alreadySeen {
					seen[typed.class] = struct{}{}
					classes = append(classes, typed.class)
				}
			}
			return
		}
		type multiUnwrapper interface {
			Unwrap() []error
		}
		if unwrapped, ok := current.(multiUnwrapper); ok {
			for _, child := range unwrapped.Unwrap() {
				walk(child)
			}
			return
		}
		type singleUnwrapper interface {
			Unwrap() error
		}
		if unwrapped, ok := current.(singleUnwrapper); ok {
			walk(unwrapped.Unwrap())
		}
	}
	walk(err)
	return classes
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
	snapshots map[mountID]syncengine.ShortcutChildWorkSnapshot,
) ([]shortcutChildArtifactCleanup, error) {
	if len(snapshots) == 0 {
		return nil, nil
	}

	cleanups := make([]shortcutChildArtifactCleanup, 0)
	for parentID, snapshot := range snapshots {
		namespaceID := parentID.String()
		if snapshot.NamespaceID != "" {
			namespaceID = snapshot.NamespaceID
		}
		for i := range snapshot.CleanupCommands {
			request := snapshot.CleanupCommands[i]
			if request.AckRef.IsZero() {
				return nil, fmt.Errorf("parent cleanup request from %s is missing acknowledgement reference", namespaceID)
			}
			reason := request.Reason
			if reason == "" {
				return nil, fmt.Errorf("parent cleanup request for child mount %s is missing cleanup reason", request.ChildMountID)
			}
			childMountID := request.ChildMountID
			if childMountID == "" {
				return nil, fmt.Errorf("parent cleanup request is missing child mount ID")
			}
			if request.LocalRoot == "" {
				return nil, fmt.Errorf("parent cleanup request for child mount %s is missing child local root", childMountID)
			}
			cleanups = append(cleanups, shortcutChildArtifactCleanup{
				mountID:     childMountID,
				namespaceID: namespaceID,
				ackRef:      request.AckRef,
				localRoot:   request.LocalRoot,
				reason:      reason,
			})
		}
	}
	sort.Slice(cleanups, func(i, j int) bool {
		if cleanups[i].mountID == cleanups[j].mountID {
			return cleanups[i].reason < cleanups[j].reason
		}
		return cleanups[i].mountID < cleanups[j].mountID
	})
	return cleanups, nil
}

func (o *Orchestrator) purgeShortcutChildArtifactsForDecisions(
	ctx context.Context,
	decisions *runtimeWorkSet,
	parentAckers map[mountID]shortcutChildAckHandle,
	childWork *parentChildWorkSnapshots,
) error {
	if decisions == nil {
		return nil
	}
	if len(decisions.CleanupChildren) == 0 {
		if decisions.CleanupScopeAllParents {
			o.clearShortcutCleanupDiagnostics(shortcutChildArtifactCleanupSourcePublished)
		}
		return nil
	}
	return o.purgeAndAcknowledgeShortcutChildArtifacts(
		ctx,
		shortcutChildArtifactCleanupSourcePublished,
		decisions.CleanupChildren,
		parentAckers,
		childWork,
	)
}

func (o *Orchestrator) purgeAndAcknowledgeShortcutChildArtifacts(
	ctx context.Context,
	source shortcutChildArtifactCleanupSource,
	cleanups []shortcutChildArtifactCleanup,
	parentAckers map[mountID]shortcutChildAckHandle,
	childWork *parentChildWorkSnapshots,
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
		if err := o.acknowledgeShortcutChildArtifactCleanups(ctx, purged, parentAckers, childWork); err != nil {
			errs = append(errs, err)
		}
	}
	err := errors.Join(errs...)
	if err != nil {
		o.recordShortcutCleanupDiagnostic(source, err)
	} else {
		o.clearShortcutCleanupDiagnostics(source)
	}
	return err
}

func (o *Orchestrator) recordShortcutCleanupDiagnostic(
	source shortcutChildArtifactCleanupSource,
	err error,
) {
	if o == nil || err == nil {
		return
	}
	classes := shortcutChildArtifactCleanupClasses(err)
	if len(classes) == 0 {
		classes = []shortcutChildArtifactCleanupFailureClass{shortcutChildArtifactCleanupSetupFailure}
	}
	o.statusMu.Lock()
	defer o.statusMu.Unlock()
	for _, class := range classes {
		phase := shortcutCleanupDiagnosticArtifactsRemaining
		if class == shortcutChildArtifactCleanupParentAckFailure {
			phase = shortcutCleanupDiagnosticParentAckFailed
		}
		o.shortcutCleanupDiagnostics = append(o.shortcutCleanupDiagnostics, synccontrol.ShortcutCleanupDiagnostic{
			Source:  string(source),
			Class:   string(class),
			Phase:   phase,
			Message: err.Error(),
		})
	}
	if overflow := len(o.shortcutCleanupDiagnostics) - shortcutCleanupDiagnosticLimit; overflow > 0 {
		o.shortcutCleanupDiagnostics = append([]synccontrol.ShortcutCleanupDiagnostic(nil), o.shortcutCleanupDiagnostics[overflow:]...)
	}
}

func (o *Orchestrator) clearShortcutCleanupDiagnostics(source shortcutChildArtifactCleanupSource) {
	if o == nil {
		return
	}
	o.statusMu.Lock()
	defer o.statusMu.Unlock()
	if len(o.shortcutCleanupDiagnostics) == 0 {
		return
	}
	kept := o.shortcutCleanupDiagnostics[:0]
	for _, diagnostic := range o.shortcutCleanupDiagnostics {
		if diagnostic.Source == string(source) {
			continue
		}
		kept = append(kept, diagnostic)
	}
	o.shortcutCleanupDiagnostics = kept
}

func (o *Orchestrator) shortcutCleanupDiagnosticSnapshot() []synccontrol.ShortcutCleanupDiagnostic {
	if o == nil {
		return nil
	}
	o.statusMu.RLock()
	defer o.statusMu.RUnlock()
	return append([]synccontrol.ShortcutCleanupDiagnostic(nil), o.shortcutCleanupDiagnostics...)
}

func (o *Orchestrator) acknowledgeShortcutChildArtifactCleanups(
	ctx context.Context,
	cleanups []shortcutChildArtifactCleanup,
	parentAckers map[mountID]shortcutChildAckHandle,
	childWork *parentChildWorkSnapshots,
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
			Ref: cleanup.ackRef,
		})
		if err != nil {
			errs = append(errs, classifiedShortcutChildCleanupError(
				shortcutChildArtifactCleanupParentAckFailure,
				fmt.Sprintf("acknowledge child artifact cleanup for mount %s", cleanup.mountID),
				err,
			))
			continue
		}
		childWork.receive(parentID, snapshot)
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
