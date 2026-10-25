package sync

import (
	"path"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

type shortcutRootIssueClass string

const (
	shortcutRootIssueNone                       shortcutRootIssueClass = ""
	shortcutRootIssueTargetUnavailable          shortcutRootIssueClass = "target_unavailable"
	shortcutRootIssueLocalRootUnavailable       shortcutRootIssueClass = "local_root_unavailable"
	shortcutRootIssueBlockedPath                shortcutRootIssueClass = "blocked_path"
	shortcutRootIssueRenameAmbiguous            shortcutRootIssueClass = "rename_ambiguous"
	shortcutRootIssueAliasMutationBlocked       shortcutRootIssueClass = "alias_mutation_blocked"
	shortcutRootIssueRemovedFinalDrain          shortcutRootIssueClass = "removed_final_drain"
	shortcutRootIssueRemovedReleasePending      shortcutRootIssueClass = "removed_release_pending"
	shortcutRootIssueRemovedCleanupBlocked      shortcutRootIssueClass = "removed_cleanup_blocked"
	shortcutRootIssueRemovedChildCleanupPending shortcutRootIssueClass = "removed_child_cleanup_pending"
	shortcutRootIssueSamePathReplacementWaiting shortcutRootIssueClass = "same_path_replacement_waiting"
	shortcutRootIssueDuplicateTarget            shortcutRootIssueClass = "duplicate_target"
	shortcutRootIssueParentRecovery             shortcutRootIssueClass = "parent_recovery"
)

type shortcutRootRecoveryClass string

const (
	shortcutRootRecoveryNone                       shortcutRootRecoveryClass = ""
	shortcutRootRecoveryRestoreTargetOrRemoveAlias shortcutRootRecoveryClass = "restore_target_or_remove_alias"
	shortcutRootRecoveryRestoreLocalRootOrDiscard  shortcutRootRecoveryClass = "restore_local_root_or_discard"
	shortcutRootRecoveryClearBlockedPath           shortcutRootRecoveryClass = "clear_blocked_path"
	shortcutRootRecoveryDisambiguateAliasRename    shortcutRootRecoveryClass = "disambiguate_alias_rename"
	shortcutRootRecoveryFixAliasMutation           shortcutRootRecoveryClass = "fix_alias_mutation"
	shortcutRootRecoveryRestoreTargetOrDiscard     shortcutRootRecoveryClass = "restore_target_or_discard"
	shortcutRootRecoveryWaitForRetry               shortcutRootRecoveryClass = "wait_for_retry"
	shortcutRootRecoveryRemoveDuplicateAlias       shortcutRootRecoveryClass = "remove_duplicate_alias"
)

// shortcutRootStatusMetadata is the display contract for one lifecycle
// state. DisplayState, StateReason, IssueClass and RecoveryClass are stable
// machine tokens for --json consumers. StatusLabel, Issue and RecoveryAction
// are the user-facing words: a short status label, one plain sentence saying
// what is going on, and the step that clears it, all in user vocabulary
// (shared folder, folder, OneDrive) per spec/reference/cli-output-guidelines.md.
type shortcutRootStatusMetadata struct {
	DisplayState   string
	StatusLabel    string
	StateReason    string
	IssueClass     shortcutRootIssueClass
	Issue          string
	RecoveryClass  shortcutRootRecoveryClass
	RecoveryAction string
	AutoRetry      bool
	ProtectsPath   bool
}

// ShortcutRootStatusView is one shortcut root as `status` shows it. Reason is
// the plain user-facing sentence for its state; Detail is the raw diagnostic
// recorded with the state (an error chain, for example), which the CLI shows
// only with --verbose or --debug.
type ShortcutRootStatusView struct {
	MountID                     string
	SortPath                    string
	DisplayName                 string
	DisplayLocalRoot            string
	Metadata                    shortcutRootStatusMetadata
	Reason                      string
	Detail                      string
	ProtectedCurrentLocalRoot   string
	ProtectedReservedLocalRoots []string
	WaitingReplacementPath      string
}

func shortcutRootStatusViewFromRecord(
	record *ShortcutRootRecord,
	namespaceID string,
	parentSyncRoot string,
) ShortcutRootStatusView {
	if record == nil {
		return ShortcutRootStatusView{}
	}
	normalized := normalizeShortcutRootRecord(record)
	if namespaceID == "" {
		namespaceID = normalized.NamespaceID
	}
	metadata := ShortcutRootStatus(normalized.State)
	view := ShortcutRootStatusView{
		MountID:          config.ChildMountID(namespaceID, normalized.BindingItemID),
		SortPath:         normalized.RelativeLocalPath,
		DisplayName:      shortcutRootStatusDisplayName(normalized.LocalAlias, normalized.RelativeLocalPath),
		DisplayLocalRoot: shortcutRootStatusLocalRoot(parentSyncRoot, normalized.RelativeLocalPath),
		Metadata:         metadata,
		Reason:           metadata.Issue,
		Detail:           normalized.BlockedDetail,
	}
	if normalized.Waiting != nil {
		view.WaitingReplacementPath = normalized.Waiting.RelativeLocalPath
	}
	if metadata.ProtectsPath {
		view.ProtectedCurrentLocalRoot = shortcutRootStatusLocalRoot(parentSyncRoot, normalized.RelativeLocalPath)
		reservedPaths := shortcutRootStatusReservedPaths(
			normalized.RelativeLocalPath,
			normalized.ProtectedPaths,
		)
		view.ProtectedReservedLocalRoots = shortcutRootStatusLocalRoots(parentSyncRoot, reservedPaths)
	}
	return view
}

func shortcutRootStatusViewsFromRecords(
	records []ShortcutRootRecord,
	namespaceID string,
	parentSyncRoot string,
) []ShortcutRootStatusView {
	views := make([]ShortcutRootStatusView, 0, len(records))
	for i := range records {
		views = append(views, shortcutRootStatusViewFromRecord(&records[i], namespaceID, parentSyncRoot))
	}
	return views
}

func shortcutRootStatusDisplayName(localAlias string, relativeLocalPath string) string {
	if localAlias != "" {
		return localAlias
	}
	return path.Base(relativeLocalPath)
}

func shortcutRootStatusLocalRoot(parentSyncRoot string, relativeLocalPath string) string {
	if parentSyncRoot == "" || relativeLocalPath == "" {
		return ""
	}
	return filepath.Join(parentSyncRoot, filepath.FromSlash(relativeLocalPath))
}

func shortcutRootStatusLocalRoots(parentSyncRoot string, relativePaths []string) []string {
	if parentSyncRoot == "" || len(relativePaths) == 0 {
		return nil
	}
	roots := make([]string, 0, len(relativePaths))
	for _, relativePath := range relativePaths {
		root := shortcutRootStatusLocalRoot(parentSyncRoot, relativePath)
		if root != "" {
			roots = append(roots, root)
		}
	}
	return roots
}

func shortcutRootStatusReservedPaths(current string, protected []string) []string {
	reserved := make([]string, 0, len(protected))
	for _, protectedPath := range protected {
		if protectedPath == "" || protectedPath == current {
			continue
		}
		reserved = append(reserved, protectedPath)
	}
	return reserved
}

type shortcutRootLifecycleMetadata struct {
	status           shortcutRootStatusMetadata
	protectsPath     bool
	runMode          ShortcutChildRunMode
	publishesCleanup bool
	transitions      map[shortcutRootLifecycleEvent][]ShortcutRootState
}

func ShortcutRootStatus(state ShortcutRootState) shortcutRootStatusMetadata {
	if state == "" || state == ShortcutRootStateActive {
		return shortcutRootStatusMetadata{}
	}
	if entry, ok := shortcutRootLifecycleMetadataFor(state); ok {
		return entry.status
	}
	return shortcutRootStatusMetadata{
		DisplayState:  string(state),
		StatusLabel:   "Recovering",
		StateReason:   string(state),
		IssueClass:    shortcutRootIssueParentRecovery,
		Issue:         "This shared folder is recovering; it is retried on every sync.",
		RecoveryClass: shortcutRootRecoveryWaitForRetry,
		AutoRetry:     true,
		ProtectsPath:  true,
	}
}

func shortcutRootLifecycleMetadataFor(state ShortcutRootState) (shortcutRootLifecycleMetadata, bool) {
	state = normalizeShortcutRootState(state)
	entry, ok := shortcutRootLifecycleMetadataTable()[state]
	return entry, ok
}

//nolint:funlen // The lifecycle table intentionally centralizes state metadata and legal transitions.
func shortcutRootLifecycleMetadataTable() map[ShortcutRootState]shortcutRootLifecycleMetadata {
	baseRecovery := map[shortcutRootLifecycleEvent][]ShortcutRootState{
		shortcutRootEventRemoteUpsert:          {ShortcutRootStateActive},
		shortcutRootEventRemoteDelete:          {ShortcutRootStateRemovedFinalDrain},
		shortcutRootEventRemoteUnavailable:     {ShortcutRootStateTargetUnavailable},
		shortcutRootEventCompleteOmission:      {ShortcutRootStateRemovedFinalDrain},
		shortcutRootEventProtectedPathConflict: {ShortcutRootStateBlockedPath},
		shortcutRootEventLocalRootReady:        {ShortcutRootStateActive},
		shortcutRootEventLocalPathBlocked: {
			ShortcutRootStateBlockedPath,
			ShortcutRootStateLocalRootUnavailable,
		},
		shortcutRootEventAliasMutationSucceeded: {
			ShortcutRootStateActive,
			ShortcutRootStateRemovedFinalDrain,
		},
		shortcutRootEventAliasMutationFailed:  {ShortcutRootStateAliasMutationBlocked},
		shortcutRootEventAliasRenameAmbiguous: {ShortcutRootStateRenameAmbiguous},
		shortcutRootEventDuplicateTargetDetected: {
			ShortcutRootStateDuplicateTarget,
		},
	}
	return map[ShortcutRootState]shortcutRootLifecycleMetadata{
		ShortcutRootStateActive: {
			protectsPath: true,
			runMode:      ShortcutChildRunModeNormal,
			transitions:  cloneShortcutRootTransitionTargets(baseRecovery),
		},
		ShortcutRootStateTargetUnavailable: {
			status: shortcutRootStatusMetadata{
				DisplayState:   string(ShortcutRootStateTargetUnavailable),
				StatusLabel:    "Shared folder unavailable",
				StateReason:    string(ShortcutRootStateTargetUnavailable),
				IssueClass:     shortcutRootIssueTargetUnavailable,
				Issue:          "OneDrive can't open this shared folder; it may have been unshared, moved or deleted.",
				RecoveryClass:  shortcutRootRecoveryRestoreTargetOrRemoveAlias,
				RecoveryAction: "Ask the owner to share it with you again, or remove the shortcut from your OneDrive.",
				AutoRetry:      true,
				ProtectsPath:   true,
			},
			protectsPath: true,
			transitions:  shortcutRootTransitions(baseRecovery, nil),
		},
		ShortcutRootStateLocalRootUnavailable: {
			status: shortcutRootStatusMetadata{
				DisplayState:   string(ShortcutRootStateLocalRootUnavailable),
				StatusLabel:    "Local folder unavailable",
				StateReason:    string(ShortcutRootStateLocalRootUnavailable),
				IssueClass:     shortcutRootIssueLocalRootUnavailable,
				Issue:          "This shared folder's local folder is missing or can't be read.",
				RecoveryClass:  shortcutRootRecoveryRestoreLocalRootOrDiscard,
				RecoveryAction: "Restore the local folder, or delete it to discard changes that have not synced.",
				AutoRetry:      true,
				ProtectsPath:   true,
			},
			protectsPath: true,
			transitions: shortcutRootTransitions(baseRecovery, map[shortcutRootLifecycleEvent][]ShortcutRootState{
				shortcutRootEventLocalPathBlocked: {ShortcutRootStateLocalRootUnavailable},
			}),
		},
		ShortcutRootStateBlockedPath: {
			status: shortcutRootStatusMetadata{
				DisplayState:   string(ShortcutRootStateBlockedPath),
				StatusLabel:    "Blocked",
				StateReason:    string(ShortcutRootStateBlockedPath),
				IssueClass:     shortcutRootIssueBlockedPath,
				Issue:          "Something else is in the way at this shared folder's local path.",
				RecoveryClass:  shortcutRootRecoveryClearBlockedPath,
				RecoveryAction: "Move or rename the file or folder at that path.",
				AutoRetry:      true,
				ProtectsPath:   true,
			},
			protectsPath: true,
			transitions:  shortcutRootTransitions(baseRecovery, nil),
		},
		ShortcutRootStateRenameAmbiguous: {
			status: shortcutRootStatusMetadata{
				DisplayState:   string(ShortcutRootStateRenameAmbiguous),
				StatusLabel:    "Rename unclear",
				StateReason:    string(ShortcutRootStateRenameAmbiguous),
				IssueClass:     shortcutRootIssueRenameAmbiguous,
				Issue:          "More than one renamed folder could be this shared folder.",
				RecoveryClass:  shortcutRootRecoveryDisambiguateAliasRename,
				RecoveryAction: "Keep exactly one renamed folder, or restore the original name.",
				AutoRetry:      true,
				ProtectsPath:   true,
			},
			protectsPath: true,
			transitions: shortcutRootTransitions(baseRecovery, map[shortcutRootLifecycleEvent][]ShortcutRootState{
				shortcutRootEventRemoteUnavailable:     nil,
				shortcutRootEventProtectedPathConflict: nil,
			}),
		},
		ShortcutRootStateAliasMutationBlocked: {
			status: shortcutRootStatusMetadata{
				DisplayState:   string(ShortcutRootStateAliasMutationBlocked),
				StatusLabel:    "Change not applied",
				StateReason:    string(ShortcutRootStateAliasMutationBlocked),
				IssueClass:     shortcutRootIssueAliasMutationBlocked,
				Issue:          "Renaming or deleting this shared folder locally couldn't be applied in OneDrive.",
				RecoveryClass:  shortcutRootRecoveryFixAliasMutation,
				RecoveryAction: "Check your connection and your access to the folder, or undo the local rename or delete.",
				AutoRetry:      true,
				ProtectsPath:   true,
			},
			protectsPath: true,
			transitions: shortcutRootTransitions(baseRecovery, map[shortcutRootLifecycleEvent][]ShortcutRootState{
				shortcutRootEventRemoteUnavailable:     nil,
				shortcutRootEventProtectedPathConflict: nil,
			}),
		},
		ShortcutRootStateRemovedFinalDrain: {
			status: shortcutRootStatusMetadata{
				DisplayState: string(ShortcutRootStateRemovedFinalDrain),
				StatusLabel:  "Removed, finishing sync",
				StateReason:  string(ShortcutRootStateRemovedFinalDrain),
				IssueClass:   shortcutRootIssueRemovedFinalDrain,
				Issue: "This shared folder was removed from your OneDrive; " +
					"its last local changes are syncing before the local copy is removed.",
				RecoveryClass:  shortcutRootRecoveryRestoreTargetOrDiscard,
				RecoveryAction: "Restore access to the shared folder, or delete its local folder to discard changes that have not synced.",
				AutoRetry:      true,
				ProtectsPath:   true,
			},
			protectsPath: true,
			runMode:      ShortcutChildRunModeFinalDrain,
			transitions: map[shortcutRootLifecycleEvent][]ShortcutRootState{
				shortcutRootEventRemoteUpsert:              {ShortcutRootStateActive},
				shortcutRootEventSamePathReplacement:       {ShortcutRootStateSamePathReplacementWaiting},
				shortcutRootEventChildFinalDrainClean:      {ShortcutRootStateRemovedReleasePending},
				shortcutRootEventProjectionCleanupFailed:   {ShortcutRootStateRemovedCleanupBlocked},
				shortcutRootEventWaitingReplacementPromote: {ShortcutRootStateActive},
			},
		},
		ShortcutRootStateRemovedReleasePending: {
			status: shortcutRootStatusMetadata{
				DisplayState:  string(ShortcutRootStateRemovedReleasePending),
				StatusLabel:   "Removed, cleaning up",
				StateReason:   string(ShortcutRootStateRemovedReleasePending),
				IssueClass:    shortcutRootIssueRemovedReleasePending,
				Issue:         "This shared folder was removed and its last changes have synced; its local copy is being removed.",
				RecoveryClass: shortcutRootRecoveryWaitForRetry,
				AutoRetry:     true,
				ProtectsPath:  true,
			},
			protectsPath: true,
			transitions: map[shortcutRootLifecycleEvent][]ShortcutRootState{
				shortcutRootEventProjectionCleanupFailed:    {ShortcutRootStateRemovedCleanupBlocked},
				shortcutRootEventProjectionCleanupSucceeded: {ShortcutRootStateRemovedChildCleanupPending},
				shortcutRootEventWaitingReplacementPromote:  {ShortcutRootStateActive},
			},
		},
		ShortcutRootStateRemovedCleanupBlocked: {
			status: shortcutRootStatusMetadata{
				DisplayState:   string(ShortcutRootStateRemovedCleanupBlocked),
				StatusLabel:    "Removed, cleanup blocked",
				StateReason:    string(ShortcutRootStateRemovedCleanupBlocked),
				IssueClass:     shortcutRootIssueRemovedCleanupBlocked,
				Issue:          "This shared folder was removed, but its local copy can't be removed.",
				RecoveryClass:  shortcutRootRecoveryClearBlockedPath,
				RecoveryAction: "Move or delete what is in the way at that path; the next sync finishes the cleanup.",
				AutoRetry:      true,
				ProtectsPath:   true,
			},
			protectsPath: true,
			transitions: map[shortcutRootLifecycleEvent][]ShortcutRootState{
				shortcutRootEventRemoteUpsert:               {ShortcutRootStateActive},
				shortcutRootEventSamePathReplacement:        {ShortcutRootStateSamePathReplacementWaiting},
				shortcutRootEventChildFinalDrainClean:       {ShortcutRootStateRemovedReleasePending},
				shortcutRootEventProjectionCleanupFailed:    {ShortcutRootStateRemovedCleanupBlocked},
				shortcutRootEventProjectionCleanupSucceeded: {ShortcutRootStateRemovedChildCleanupPending},
				shortcutRootEventWaitingReplacementPromote:  {ShortcutRootStateActive},
			},
		},
		ShortcutRootStateRemovedChildCleanupPending: {
			status: shortcutRootStatusMetadata{
				DisplayState:  string(ShortcutRootStateRemovedChildCleanupPending),
				StatusLabel:   "Removed, cleaning up",
				StateReason:   string(ShortcutRootStateRemovedChildCleanupPending),
				IssueClass:    shortcutRootIssueRemovedChildCleanupPending,
				Issue:         "This shared folder was removed; its local sync data is being cleaned up.",
				RecoveryClass: shortcutRootRecoveryWaitForRetry,
				AutoRetry:     true,
			},
			publishesCleanup: true,
			transitions: map[shortcutRootLifecycleEvent][]ShortcutRootState{
				shortcutRootEventRemoteUpsert:         {ShortcutRootStateActive},
				shortcutRootEventChildArtifactsPurged: {},
			},
		},
		ShortcutRootStateSamePathReplacementWaiting: {
			status: shortcutRootStatusMetadata{
				DisplayState:  string(ShortcutRootStateSamePathReplacementWaiting),
				StatusLabel:   "Waiting",
				StateReason:   string(ShortcutRootStateSamePathReplacementWaiting),
				IssueClass:    shortcutRootIssueSamePathReplacementWaiting,
				Issue:         "A new shared folder with the same name is waiting for the old one to finish syncing.",
				RecoveryClass: shortcutRootRecoveryWaitForRetry,
				AutoRetry:     true,
				ProtectsPath:  true,
			},
			protectsPath: true,
			runMode:      ShortcutChildRunModeFinalDrain,
			transitions: map[shortcutRootLifecycleEvent][]ShortcutRootState{
				shortcutRootEventRemoteUpsert:              {ShortcutRootStateActive},
				shortcutRootEventSamePathReplacement:       {ShortcutRootStateSamePathReplacementWaiting},
				shortcutRootEventChildFinalDrainClean:      {ShortcutRootStateRemovedReleasePending},
				shortcutRootEventProjectionCleanupFailed:   {ShortcutRootStateRemovedCleanupBlocked},
				shortcutRootEventWaitingReplacementPromote: {ShortcutRootStateActive},
			},
		},
		ShortcutRootStateDuplicateTarget: {
			status: shortcutRootStatusMetadata{
				DisplayState:   string(ShortcutRootStateDuplicateTarget),
				StatusLabel:    "Duplicate",
				StateReason:    string(ShortcutRootStateDuplicateTarget),
				IssueClass:     shortcutRootIssueDuplicateTarget,
				Issue:          "Another shortcut in this drive already syncs the same shared folder.",
				RecoveryClass:  shortcutRootRecoveryRemoveDuplicateAlias,
				RecoveryAction: "Remove one of the two shortcuts from your OneDrive.",
				AutoRetry:      true,
				ProtectsPath:   true,
			},
			protectsPath: true,
			transitions: map[shortcutRootLifecycleEvent][]ShortcutRootState{
				shortcutRootEventRemoteUpsert:            {ShortcutRootStateActive},
				shortcutRootEventRemoteDelete:            {ShortcutRootStateRemovedFinalDrain},
				shortcutRootEventRemoteUnavailable:       {ShortcutRootStateTargetUnavailable},
				shortcutRootEventCompleteOmission:        {ShortcutRootStateRemovedFinalDrain},
				shortcutRootEventDuplicateTargetDetected: {ShortcutRootStateDuplicateTarget},
				shortcutRootEventDuplicateTargetResolved: {ShortcutRootStateActive},
				shortcutRootEventProtectedPathConflict:   {ShortcutRootStateBlockedPath},
				shortcutRootEventLocalRootReady:          {ShortcutRootStateDuplicateTarget},
				shortcutRootEventLocalPathBlocked:        {ShortcutRootStateBlockedPath},
				shortcutRootEventAliasMutationFailed:     {ShortcutRootStateAliasMutationBlocked},
				shortcutRootEventAliasRenameAmbiguous:    {ShortcutRootStateRenameAmbiguous},
			},
		},
	}
}

func shortcutRootTransitions(
	base map[shortcutRootLifecycleEvent][]ShortcutRootState,
	overrides map[shortcutRootLifecycleEvent][]ShortcutRootState,
) map[shortcutRootLifecycleEvent][]ShortcutRootState {
	result := cloneShortcutRootTransitionTargets(base)
	for event, targets := range overrides {
		if targets == nil {
			delete(result, event)
			continue
		}
		result[event] = append([]ShortcutRootState(nil), targets...)
	}
	return result
}

func cloneShortcutRootTransitionTargets(
	transitions map[shortcutRootLifecycleEvent][]ShortcutRootState,
) map[shortcutRootLifecycleEvent][]ShortcutRootState {
	cloned := make(map[shortcutRootLifecycleEvent][]ShortcutRootState, len(transitions))
	for event, targets := range transitions {
		cloned[event] = append([]ShortcutRootState(nil), targets...)
	}
	return cloned
}
