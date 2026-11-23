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
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"
)

type shortcutChildArtifactCleanup struct {
	mountID     string
	namespaceID string
	ackRef      syncshortcut.ShortcutChildAckRef
	localRoot   string
	reason      syncshortcut.ShortcutChildArtifactCleanupReason
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
	shortcutChildArtifactCleanupUploadSessionFailure shortcutChildArtifactCleanupFailureClass = "upload_session"
	shortcutChildArtifactCleanupParentAckFailure     shortcutChildArtifactCleanupFailureClass = "parent_ack"
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

// Error reads as plain "what failed: why". The failure class is for status
// diagnostics (see shortcutChildArtifactCleanupClasses), not for the message.
func (e shortcutChildArtifactCleanupError) Error() string {
	if e.op == "" {
		return e.err.Error()
	}
	return fmt.Sprintf("%s: %v", e.op, e.err)
}

func (e shortcutChildArtifactCleanupError) Unwrap() error {
	return e.err
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
	deleteUploadSessions func(string, string) error
}

func newShortcutChildArtifactCleanupExecutor(logger *slog.Logger, dataDir string) shortcutChildArtifactCleanupExecutor {
	return shortcutChildArtifactCleanupExecutor{
		dataDir: dataDir,
		logger:  logger,
		remove:  localpath.Remove,
		deleteUploadSessions: func(childMountID string, localRoot string) error {
			sessionStore := driveops.NewSessionStore(dataDir, logger)
			_, err := sessionStore.DeleteForScope(childMountID, localRoot)
			if err != nil {
				return fmt.Errorf("delete upload sessions: %w", err)
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
	snapshots map[mountID]syncshortcut.ShortcutChildWorkSnapshot,
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
			errs = append(errs, fmt.Errorf("removed shortcut %s: %w", cleanup.localRoot, err))
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
		snapshot, err := acker.AcknowledgeChildArtifactsPurged(ctx, syncshortcut.ShortcutChildArtifactCleanupAck{
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

// shortcutChildTrash names the two trash directories of a shortcut child
// mount: its own and its parent drive's.
type shortcutChildTrash struct {
	child  string
	parent string
}

// shortcutChildTrashDirs returns the trash directories of the shortcut child
// mount childMountID in the data directory dataDir. The child's is the child
// trash (localtrash.ChildDir) of its parent drive's trash, so the parent's
// trash commands and housekeeping reach it, and the child's engine and its
// artifact purge agree on it. The parent is the configured drive the child
// mount ID names (config.ChildMountParentID), whose state lives at
// config.DriveStatePathForDataDir.
func shortcutChildTrashDirs(dataDir, childMountID string) (shortcutChildTrash, error) {
	parent, err := driveid.NewCanonicalID(config.ChildMountParentID(childMountID))
	if err != nil {
		return shortcutChildTrash{}, fmt.Errorf("find the drive whose trash child mount %s uses: %w", childMountID, err)
	}
	parentDir := localtrash.DirForStatePath(config.DriveStatePathForDataDir(dataDir, parent))
	childDir := localtrash.ChildDir(parentDir, config.MountStatePathForDataDir(dataDir, childMountID))
	if childDir == "" {
		return shortcutChildTrash{}, fmt.Errorf("find the trash folder of child mount %s: no data directory", childMountID)
	}

	return shortcutChildTrash{child: childDir, parent: parentDir}, nil
}

// handOverShortcutChildTrash moves the entries of the child's trash into its
// parent drive's trash (localtrash.Trash.HandOverTo), so they stay listed and
// restorable by the parent's trash commands and expire with the parent's own
// once nothing of the child is left to reach them.
func (e shortcutChildArtifactCleanupExecutor) handOverShortcutChildTrash(childMountID string) error {
	trash, err := shortcutChildTrashDirs(e.dataDir, childMountID)
	if err != nil {
		return err
	}
	moved, err := localtrash.Open(trash.child, nil).HandOverTo(localtrash.Open(trash.parent, nil))
	if err != nil {
		return fmt.Errorf("hand its trash over to the drive's trash: %w", err)
	}
	if moved > 0 && e.logger != nil {
		e.logger.Info("handed a shortcut child's trash over to its drive's trash",
			slog.String("mount_id", childMountID), slog.Int("entries", moved))
	}

	return nil
}

// purge deletes a retired shortcut child's state DB family and unfinished
// uploads, after it has handed the child's trash over to the parent drive's
// trash (handOverShortcutChildTrash). A hand-over that fails stops the purge
// before anything else of the child is removed and fails it as a remaining
// state artifact, so the parent publishes the cleanup again: the purge never
// deletes a trash entry and never leaves a trash that no command reaches. A
// child mount has no catalog record: the catalog holds only canonical drive
// IDs, which a child mount ID is not.
//
// purge holds the child's store lock for the whole cleanup
// (syncengine.LockDriveForOfflineChange): a store another engine still has
// open, such as another configuration's child of the same shortcut sharing
// the data directory, refuses it as a remaining state artifact and nothing of
// the child is touched, so the parent publishes the cleanup again and a later
// attempt finds the store free. Only the store is locked, not the child's
// folder: a replacement shortcut's child may already sync that folder, and
// its folder lock is not this store's.
func (e shortcutChildArtifactCleanupExecutor) purge(
	ctx context.Context,
	scope shortcutChildArtifactScope,
) (err error) {
	childMountID := strings.TrimSpace(scope.mountID)
	if childMountID == "" {
		return nil
	}
	if ctx.Err() != nil {
		return classifiedShortcutChildCleanupError(
			shortcutChildArtifactCleanupSetupFailure,
			"purge shortcut child artifacts",
			ctx.Err(),
		)
	}
	if !config.IsChildMountID(childMountID) {
		return nil
	}
	if configErr := e.requireConfigured(); configErr != nil {
		return configErr
	}

	statePath := config.MountStatePathForDataDir(e.dataDir, childMountID)
	release, lockErr := syncengine.LockDriveForOfflineChange(ctx, e.dataDir, "", statePath, e.logger)
	if lockErr != nil {
		return classifiedShortcutChildCleanupError(
			shortcutChildArtifactCleanupStateArtifactFailure,
			"delete its sync state",
			lockErr,
		)
	}
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			err = errors.Join(err, classifiedShortcutChildCleanupError(
				shortcutChildArtifactCleanupStateArtifactFailure,
				"release its sync state lock",
				releaseErr,
			))
		}
	}()

	if handOverErr := e.handOverShortcutChildTrash(childMountID); handOverErr != nil {
		return classifiedShortcutChildCleanupError(
			shortcutChildArtifactCleanupStateArtifactFailure,
			"keep its trash",
			handOverErr,
		)
	}

	var errs []error
	for _, path := range config.StateDBFamily(statePath) {
		if removeErr := e.remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			errs = append(errs, classifiedShortcutChildCleanupError(
				shortcutChildArtifactCleanupStateArtifactFailure,
				"delete its sync state",
				removeErr,
			))
		}
	}
	if sessionErr := e.deleteUploadSessions(childMountID, scope.localRoot); sessionErr != nil {
		errs = append(errs, classifiedShortcutChildCleanupError(
			shortcutChildArtifactCleanupUploadSessionFailure,
			"discard its unfinished uploads",
			sessionErr,
		))
	}
	if cleanupErr := errors.Join(errs...); cleanupErr != nil {
		return cleanupErr
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
	if e.remove == nil || e.deleteUploadSessions == nil {
		return classifiedShortcutChildCleanupError(
			shortcutChildArtifactCleanupSetupFailure,
			"configure cleanup executor",
			fmt.Errorf("cleanup executor is not configured"),
		)
	}
	return nil
}
