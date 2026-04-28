package sync

import (
	"path"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

type ShortcutRootIssueClass string

const (
	ShortcutRootIssueNone                       ShortcutRootIssueClass = ""
	ShortcutRootIssueTargetUnavailable          ShortcutRootIssueClass = "target_unavailable"
	ShortcutRootIssueLocalRootUnavailable       ShortcutRootIssueClass = "local_root_unavailable"
	ShortcutRootIssueBlockedPath                ShortcutRootIssueClass = "blocked_path"
	ShortcutRootIssueRenameAmbiguous            ShortcutRootIssueClass = "rename_ambiguous"
	ShortcutRootIssueAliasMutationBlocked       ShortcutRootIssueClass = "alias_mutation_blocked"
	ShortcutRootIssueRemovedFinalDrain          ShortcutRootIssueClass = "removed_final_drain"
	ShortcutRootIssueRemovedReleasePending      ShortcutRootIssueClass = "removed_release_pending"
	ShortcutRootIssueRemovedCleanupBlocked      ShortcutRootIssueClass = "removed_cleanup_blocked"
	ShortcutRootIssueRemovedChildCleanupPending ShortcutRootIssueClass = "removed_child_cleanup_pending"
	ShortcutRootIssueSamePathReplacementWaiting ShortcutRootIssueClass = "same_path_replacement_waiting"
	ShortcutRootIssueDuplicateTarget            ShortcutRootIssueClass = "duplicate_target"
	ShortcutRootIssueParentRecovery             ShortcutRootIssueClass = "parent_recovery"
)

type ShortcutRootRecoveryClass string

const (
	ShortcutRootRecoveryNone                       ShortcutRootRecoveryClass = ""
	ShortcutRootRecoveryRestoreTargetOrRemoveAlias ShortcutRootRecoveryClass = "restore_target_or_remove_alias"
	ShortcutRootRecoveryRestoreLocalRootOrDiscard  ShortcutRootRecoveryClass = "restore_local_root_or_discard"
	ShortcutRootRecoveryClearBlockedPath           ShortcutRootRecoveryClass = "clear_blocked_path"
	ShortcutRootRecoveryDisambiguateAliasRename    ShortcutRootRecoveryClass = "disambiguate_alias_rename"
	ShortcutRootRecoveryFixAliasMutation           ShortcutRootRecoveryClass = "fix_alias_mutation"
	ShortcutRootRecoveryRestoreTargetOrDiscard     ShortcutRootRecoveryClass = "restore_target_or_discard"
	ShortcutRootRecoveryWaitForRetry               ShortcutRootRecoveryClass = "wait_for_retry"
	ShortcutRootRecoveryRemoveDuplicateAlias       ShortcutRootRecoveryClass = "remove_duplicate_alias"
)

type ShortcutRootStatusMetadata struct {
	DisplayState   string
	StateReason    string
	IssueClass     ShortcutRootIssueClass
	Issue          string
	RecoveryClass  ShortcutRootRecoveryClass
	RecoveryAction string
	AutoRetry      bool
	ProtectsPath   bool
}

type ShortcutRootStatusView struct {
	MountID                     string
	SortPath                    string
	DisplayName                 string
	DisplayLocalRoot            string
	Metadata                    ShortcutRootStatusMetadata
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
	status           ShortcutRootStatusMetadata
	protectsPath     bool
	runMode          ShortcutChildRunMode
	publishesCleanup bool
	transitions      map[shortcutRootLifecycleEvent][]ShortcutRootState
}

func ShortcutRootStatus(state ShortcutRootState) ShortcutRootStatusMetadata {
	if state == "" || state == ShortcutRootStateActive {
		return ShortcutRootStatusMetadata{}
	}
	if entry, ok := shortcutRootLifecycleMetadataFor(state); ok {
		return entry.status
	}
	return ShortcutRootStatusMetadata{
		DisplayState:  string(state),
		StateReason:   string(state),
		IssueClass:    ShortcutRootIssueParentRecovery,
		Issue:         "The shortcut alias is waiting for parent-engine recovery.",
		RecoveryClass: ShortcutRootRecoveryWaitForRetry,
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
			status: ShortcutRootStatusMetadata{
				DisplayState:   string(ShortcutRootStateTargetUnavailable),
				StateReason:    string(ShortcutRootStateTargetUnavailable),
				IssueClass:     ShortcutRootIssueTargetUnavailable,
				Issue:          "The shortcut target is unavailable.",
				RecoveryClass:  ShortcutRootRecoveryRestoreTargetOrRemoveAlias,
				RecoveryAction: "Restore target access or remove the shortcut alias.",
				AutoRetry:      true,
				ProtectsPath:   true,
			},
			protectsPath: true,
			transitions:  shortcutRootTransitions(baseRecovery, nil),
		},
		ShortcutRootStateLocalRootUnavailable: {
			status: ShortcutRootStatusMetadata{
				DisplayState:   string(ShortcutRootStateLocalRootUnavailable),
				StateReason:    string(ShortcutRootStateLocalRootUnavailable),
				IssueClass:     ShortcutRootIssueLocalRootUnavailable,
				Issue:          "The shortcut alias local root is unavailable.",
				RecoveryClass:  ShortcutRootRecoveryRestoreLocalRootOrDiscard,
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
			status: ShortcutRootStatusMetadata{
				DisplayState:   string(ShortcutRootStateBlockedPath),
				StateReason:    string(ShortcutRootStateBlockedPath),
				IssueClass:     ShortcutRootIssueBlockedPath,
				Issue:          "The shortcut alias path is blocked.",
				RecoveryClass:  ShortcutRootRecoveryClearBlockedPath,
				RecoveryAction: "Clear the blocking local path.",
				AutoRetry:      true,
				ProtectsPath:   true,
			},
			protectsPath: true,
			transitions:  shortcutRootTransitions(baseRecovery, nil),
		},
		ShortcutRootStateRenameAmbiguous: {
			status: ShortcutRootStatusMetadata{
				DisplayState:   string(ShortcutRootStateRenameAmbiguous),
				StateReason:    string(ShortcutRootStateRenameAmbiguous),
				IssueClass:     ShortcutRootIssueRenameAmbiguous,
				Issue:          "Multiple same-folder shortcut alias rename candidates were found.",
				RecoveryClass:  ShortcutRootRecoveryDisambiguateAliasRename,
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
			status: ShortcutRootStatusMetadata{
				DisplayState:   string(ShortcutRootStateAliasMutationBlocked),
				StateReason:    string(ShortcutRootStateAliasMutationBlocked),
				IssueClass:     ShortcutRootIssueAliasMutationBlocked,
				Issue:          "The parent engine cannot update the shortcut alias in OneDrive.",
				RecoveryClass:  ShortcutRootRecoveryFixAliasMutation,
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
			status: ShortcutRootStatusMetadata{
				DisplayState:   string(ShortcutRootStateRemovedFinalDrain),
				StateReason:    string(ShortcutRootStateRemovedFinalDrain),
				IssueClass:     ShortcutRootIssueRemovedFinalDrain,
				Issue:          "The shortcut alias was removed; child sync is finishing before release.",
				RecoveryClass:  ShortcutRootRecoveryRestoreTargetOrDiscard,
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
			status: ShortcutRootStatusMetadata{
				DisplayState:  string(ShortcutRootStateRemovedReleasePending),
				StateReason:   string(ShortcutRootStateRemovedReleasePending),
				IssueClass:    ShortcutRootIssueRemovedReleasePending,
				Issue:         "Child sync finished; the parent engine is releasing the protected shortcut alias path.",
				RecoveryClass: ShortcutRootRecoveryWaitForRetry,
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
			status: ShortcutRootStatusMetadata{
				DisplayState:   string(ShortcutRootStateRemovedCleanupBlocked),
				StateReason:    string(ShortcutRootStateRemovedCleanupBlocked),
				IssueClass:     ShortcutRootIssueRemovedCleanupBlocked,
				Issue:          "The parent engine cannot release the protected shortcut alias path.",
				RecoveryClass:  ShortcutRootRecoveryClearBlockedPath,
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
			status: ShortcutRootStatusMetadata{
				DisplayState:  string(ShortcutRootStateRemovedChildCleanupPending),
				StateReason:   string(ShortcutRootStateRemovedChildCleanupPending),
				IssueClass:    ShortcutRootIssueRemovedChildCleanupPending,
				Issue:         "The shortcut alias was released; child cleanup is finishing.",
				RecoveryClass: ShortcutRootRecoveryWaitForRetry,
				AutoRetry:     true,
			},
			publishesCleanup: true,
			transitions: map[shortcutRootLifecycleEvent][]ShortcutRootState{
				shortcutRootEventRemoteUpsert:         {ShortcutRootStateActive},
				shortcutRootEventChildArtifactsPurged: {},
			},
		},
		ShortcutRootStateSamePathReplacementWaiting: {
			status: ShortcutRootStatusMetadata{
				DisplayState:  string(ShortcutRootStateSamePathReplacementWaiting),
				StateReason:   string(ShortcutRootStateSamePathReplacementWaiting),
				IssueClass:    ShortcutRootIssueSamePathReplacementWaiting,
				Issue:         "A new shortcut is waiting for the old child sync to finish.",
				RecoveryClass: ShortcutRootRecoveryWaitForRetry,
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
			status: ShortcutRootStatusMetadata{
				DisplayState:   string(ShortcutRootStateDuplicateTarget),
				StateReason:    string(ShortcutRootStateDuplicateTarget),
				IssueClass:     ShortcutRootIssueDuplicateTarget,
				Issue:          "Another shortcut alias in this parent already projects the same target.",
				RecoveryClass:  ShortcutRootRecoveryRemoveDuplicateAlias,
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
