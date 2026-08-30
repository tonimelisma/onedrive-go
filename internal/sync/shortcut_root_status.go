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

type shortcutRootStatusMetadata struct {
	DisplayState   string
	StateReason    string
	IssueClass     shortcutRootIssueClass
	Issue          string
	RecoveryClass  shortcutRootRecoveryClass
	RecoveryAction string
	AutoRetry      bool
	ProtectsPath   bool
}

type ShortcutRootStatusView struct {
	MountID                     string
	SortPath                    string
	DisplayName                 string
	DisplayLocalRoot            string
	Metadata                    shortcutRootStatusMetadata
	StateDetail                 string
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
		StateDetail:      metadata.Issue,
	}
	if normalized.BlockedDetail != "" {
		view.StateDetail = normalized.BlockedDetail
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
		StateReason:   string(state),
		IssueClass:    shortcutRootIssueParentRecovery,
		Issue:         "The shortcut alias is waiting for parent-engine recovery.",
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
				StateReason:    string(ShortcutRootStateTargetUnavailable),
				IssueClass:     shortcutRootIssueTargetUnavailable,
				Issue:          "The shortcut target is unavailable.",
				RecoveryClass:  shortcutRootRecoveryRestoreTargetOrRemoveAlias,
				RecoveryAction: "Restore target access or remove the shortcut alias.",
				AutoRetry:      true,
				ProtectsPath:   true,
			},
			protectsPath: true,
			transitions:  shortcutRootTransitions(baseRecovery, nil),
		},
		ShortcutRootStateLocalRootUnavailable: {
			status: shortcutRootStatusMetadata{
				DisplayState:   string(ShortcutRootStateLocalRootUnavailable),
				StateReason:    string(ShortcutRootStateLocalRootUnavailable),
				IssueClass:     shortcutRootIssueLocalRootUnavailable,
				Issue:          "The shortcut alias local root is unavailable.",
				RecoveryClass:  shortcutRootRecoveryRestoreLocalRootOrDiscard,
				RecoveryAction: "Restore the local shortcut directory or delete it to discard unresolved local state.",
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
				StateReason:    string(ShortcutRootStateBlockedPath),
				IssueClass:     shortcutRootIssueBlockedPath,
				Issue:          "The shortcut alias path is blocked.",
				RecoveryClass:  shortcutRootRecoveryClearBlockedPath,
				RecoveryAction: "Clear the blocking local path.",
				AutoRetry:      true,
				ProtectsPath:   true,
			},
			protectsPath: true,
			transitions:  shortcutRootTransitions(baseRecovery, nil),
		},
		ShortcutRootStateRenameAmbiguous: {
			status: shortcutRootStatusMetadata{
				DisplayState:   string(ShortcutRootStateRenameAmbiguous),
				StateReason:    string(ShortcutRootStateRenameAmbiguous),
				IssueClass:     shortcutRootIssueRenameAmbiguous,
				Issue:          "Multiple same-folder shortcut alias rename candidates were found.",
				RecoveryClass:  shortcutRootRecoveryDisambiguateAliasRename,
				RecoveryAction: "Keep exactly one renamed shortcut alias or restore the original name.",
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
				StateReason:    string(ShortcutRootStateAliasMutationBlocked),
				IssueClass:     shortcutRootIssueAliasMutationBlocked,
				Issue:          "The parent engine cannot update the shortcut alias in OneDrive.",
				RecoveryClass:  shortcutRootRecoveryFixAliasMutation,
				RecoveryAction: "Fix account, network, or permission access, or restore the local alias.",
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
				DisplayState:   string(ShortcutRootStateRemovedFinalDrain),
				StateReason:    string(ShortcutRootStateRemovedFinalDrain),
				IssueClass:     shortcutRootIssueRemovedFinalDrain,
				Issue:          "The shortcut alias was removed; child sync is finishing before release.",
				RecoveryClass:  shortcutRootRecoveryRestoreTargetOrDiscard,
				RecoveryAction: "Restore shared-folder access, or delete the local shortcut directory to discard dirty local state.",
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
				StateReason:   string(ShortcutRootStateRemovedReleasePending),
				IssueClass:    shortcutRootIssueRemovedReleasePending,
				Issue:         "Child sync finished; the parent engine is releasing the protected shortcut alias path.",
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
				StateReason:    string(ShortcutRootStateRemovedCleanupBlocked),
				IssueClass:     shortcutRootIssueRemovedCleanupBlocked,
				Issue:          "The parent engine cannot release the protected shortcut alias path.",
				RecoveryClass:  shortcutRootRecoveryClearBlockedPath,
				RecoveryAction: "Clear the local filesystem blocker.",
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
				StateReason:   string(ShortcutRootStateRemovedChildCleanupPending),
				IssueClass:    shortcutRootIssueRemovedChildCleanupPending,
				Issue:         "The shortcut alias was released; child cleanup is finishing.",
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
				StateReason:   string(ShortcutRootStateSamePathReplacementWaiting),
				IssueClass:    shortcutRootIssueSamePathReplacementWaiting,
				Issue:         "A new shortcut is waiting for the old child sync to finish.",
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
				StateReason:    string(ShortcutRootStateDuplicateTarget),
				IssueClass:     shortcutRootIssueDuplicateTarget,
				Issue:          "Another shortcut alias in this parent already projects the same target.",
				RecoveryClass:  shortcutRootRecoveryRemoveDuplicateAlias,
				RecoveryAction: "Remove or rename the duplicate shortcut alias.",
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
