package sync

import (
	"fmt"
	"path"
	"slices"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

type shortcutRootTopologyPlan struct {
	Records []ShortcutRootRecord
	Changed bool
}

type shortcutRootDrainAckPlan struct {
	Records []ShortcutRootRecord
	Changed bool
}

type shortcutRootArtifactCleanupAckPlan struct {
	Records []ShortcutRootRecord
	Changed bool
}

// This file is the functional core for parent-owned shortcut-root lifecycle.
// Engine code gathers remote, filesystem, child-drain, and cleanup facts; these
// helpers turn those facts into next durable records without performing I/O.

type shortcutRootMissingAliasAction string

const (
	shortcutRootMissingAliasNoop            shortcutRootMissingAliasAction = ""
	shortcutRootMissingAliasDelete          shortcutRootMissingAliasAction = "delete_alias"
	shortcutRootMissingAliasRename          shortcutRootMissingAliasAction = "rename_alias"
	shortcutRootMissingAliasMoveProjection  shortcutRootMissingAliasAction = "move_projection"
	shortcutRootMissingAliasRenameAmbiguous shortcutRootMissingAliasAction = "rename_ambiguous"
)

type shortcutRootMissingAliasPlan struct {
	Action           shortcutRootMissingAliasAction
	Mutation         shortcutAliasMutation
	FromRelativePath string
	ToRelativePath   string
	CandidatePath    string
	Next             ShortcutRootRecord
	Keep             bool
	Changed          bool
}

//nolint:gocritic // ShortcutRootRecord is an immutable planner value at this boundary.
func planMissingMaterializedShortcutRoot(
	record ShortcutRootRecord,
	relativePath string,
	candidates []string,
) shortcutRootMissingAliasPlan {
	record = normalizeShortcutRootRecord(record)
	if previousPath, ok := previousProtectedProjectionCandidate(&record, candidates); ok {
		return shortcutRootMissingAliasPlan{
			Action:           shortcutRootMissingAliasMoveProjection,
			FromRelativePath: previousPath,
			ToRelativePath:   relativePath,
			Keep:             true,
		}
	}
	switch len(candidates) {
	case 0:
		return shortcutRootMissingAliasPlan{
			Action: shortcutRootMissingAliasDelete,
			Mutation: shortcutAliasMutation{
				Kind:          shortcutAliasMutationDelete,
				BindingItemID: record.BindingItemID,
			},
		}
	case 1:
		alias := path.Base(candidates[0])
		return shortcutRootMissingAliasPlan{
			Action:        shortcutRootMissingAliasRename,
			CandidatePath: candidates[0],
			Mutation: shortcutAliasMutation{
				Kind:              shortcutAliasMutationRename,
				BindingItemID:     record.BindingItemID,
				RelativeLocalPath: candidates[0],
				LocalAlias:        alias,
			},
			Keep: true,
		}
	default:
		next := plannedShortcutRootTransition(record,
			shortcutRootEventAliasRenameAmbiguous,
			ShortcutRootStateRenameAmbiguous,
			"multiple same-parent shortcut alias rename candidates",
		)
		next.ProtectedPaths = appendUniqueProtectedRootPaths(next.ProtectedPaths, candidates...)
		return shortcutRootMissingAliasPlan{
			Action:  shortcutRootMissingAliasRenameAmbiguous,
			Next:    next,
			Keep:    true,
			Changed: true,
		}
	}
}

//nolint:gocritic // ShortcutRootRecord is an immutable planner value at this boundary.
func planMissingAliasMutationFailure(
	record ShortcutRootRecord,
	candidatePath string,
	err error,
) ShortcutRootRecord {
	next := aliasMutationBlockedShortcutRoot(record, err)
	if candidatePath != "" {
		next.ProtectedPaths = appendUniqueProtectedRootPaths(next.ProtectedPaths, candidatePath)
	}
	return next
}

//nolint:gocritic // ShortcutRootRecord is an immutable planner value at this boundary.
func planMissingAliasRenameSuccess(
	record ShortcutRootRecord,
	candidatePath string,
	identity synctree.FileIdentity,
) ShortcutRootRecord {
	next := record
	next.RelativeLocalPath = candidatePath
	next.LocalAlias = path.Base(candidatePath)
	next = plannedShortcutRootTransition(next,
		shortcutRootEventLocalRootReady,
		ShortcutRootStateActive,
		"",
	)
	next.LocalRootIdentity = &identity
	next.ProtectedPaths = protectedPathsForShortcutRoot(next.RelativeLocalPath, append(record.ProtectedPaths, record.RelativeLocalPath))
	return next
}

//nolint:gocritic // ShortcutRootRecord is an immutable planner value at this boundary.
func planShortcutProjectionMoveSuccess(
	record ShortcutRootRecord,
	identity synctree.FileIdentity,
) ShortcutRootRecord {
	next := record
	next = plannedShortcutRootTransition(next,
		shortcutRootEventLocalRootReady,
		ShortcutRootStateActive,
		"",
	)
	next.LocalRootIdentity = &identity
	next.ProtectedPaths = protectedPathsForShortcutRoot(next.RelativeLocalPath, nil)
	return next
}

//nolint:funlen,gocyclo // Topology planning keeps the state transition table in one deterministic pass.
func planShortcutRootTopology(
	current []ShortcutRootRecord,
	batch shortcutTopologyBatch,
) shortcutRootTopologyPlan {
	byBinding := make(map[string]ShortcutRootRecord, len(current))
	for i := range current {
		record := normalizeShortcutRootRecord(current[i])
		if record.BindingItemID == "" {
			continue
		}
		byBinding[record.BindingItemID] = record
	}

	changed := false
	for i := range batch.Deletes {
		record, ok := byBinding[batch.Deletes[i].BindingItemID]
		if !ok {
			continue
		}
		next := plannedShortcutRootTransition(record,
			shortcutRootEventRemoteDelete,
			ShortcutRootStateRemovedFinalDrain,
			"",
		)
		next.ProtectedPaths = protectedPathsForShortcutRoot(next.RelativeLocalPath, next.ProtectedPaths)
		if !shortcutRootRecordsEqual(record, next) {
			byBinding[next.BindingItemID] = next
			changed = true
		}
	}

	for i := range batch.Unavailable {
		fact := batch.Unavailable[i]
		next := shortcutRootRecordFromUnavailable(batch.NamespaceID, fact)
		if existing, ok := byBinding[fact.BindingItemID]; ok {
			next.LocalRootIdentity = existing.LocalRootIdentity
			next.Waiting = cloneShortcutRootReplacement(existing.Waiting)
			transitioned := plannedShortcutRootTransition(existing,
				shortcutRootEventRemoteUnavailable,
				ShortcutRootStateTargetUnavailable,
				fact.Reason,
			)
			next.State = transitioned.State
			next.BlockedDetail = transitioned.BlockedDetail
		}
		changed = upsertShortcutRootRecord(byBinding, next) || changed
	}

	for i := range batch.Upserts {
		fact := batch.Upserts[i]
		next := shortcutRootRecordFromUpsert(batch.NamespaceID, fact)
		if owner, found := samePathRetiringShortcutRoot(byBinding, next.RelativeLocalPath, next.BindingItemID); found {
			waiting := ShortcutRootReplacement{
				BindingItemID:     next.BindingItemID,
				RelativeLocalPath: next.RelativeLocalPath,
				LocalAlias:        next.LocalAlias,
				RemoteDriveID:     next.RemoteDriveID,
				RemoteItemID:      next.RemoteItemID,
				RemoteIsFolder:    next.RemoteIsFolder,
			}
			owner.Waiting = &waiting
			owner = plannedShortcutRootTransition(owner,
				shortcutRootEventSamePathReplacement,
				ShortcutRootStateSamePathReplacementWaiting,
				owner.BlockedDetail,
			)
			owner.ProtectedPaths = protectedPathsForShortcutRoot(owner.RelativeLocalPath, owner.ProtectedPaths)
			byBinding[owner.BindingItemID] = owner
			changed = true
			continue
		}
		if _, found := activeProtectedShortcutRootForPath(byBinding, next.RelativeLocalPath, next.BindingItemID); found {
			next = plannedShortcutRootTransition(next,
				shortcutRootEventProtectedPathConflict,
				ShortcutRootStateBlockedPath,
				"shortcut alias path is protected by another shortcut root",
			)
			next.ProtectedPaths = protectedPathsForShortcutRoot(next.RelativeLocalPath, next.ProtectedPaths)
			changed = upsertShortcutRootRecord(byBinding, next) || changed
			continue
		}
		if existing, ok := byBinding[fact.BindingItemID]; ok {
			transitioned := plannedShortcutRootTransition(existing,
				shortcutRootEventRemoteUpsert,
				ShortcutRootStateActive,
				"",
			)
			next.State = transitioned.State
			next.BlockedDetail = transitioned.BlockedDetail
			next.LocalRootIdentity = existing.LocalRootIdentity
			if existing.RelativeLocalPath != "" && existing.RelativeLocalPath != next.RelativeLocalPath {
				next.ProtectedPaths = protectedPathsForShortcutRoot(
					next.RelativeLocalPath,
					append(existing.ProtectedPaths, existing.RelativeLocalPath),
				)
			}
		}
		changed = upsertShortcutRootRecord(byBinding, next) || changed
	}

	if batch.Kind == shortcutTopologyObservationComplete {
		seen := make(map[string]struct{}, len(batch.Upserts)+len(batch.Unavailable))
		for i := range batch.Upserts {
			seen[batch.Upserts[i].BindingItemID] = struct{}{}
		}
		for i := range batch.Unavailable {
			seen[batch.Unavailable[i].BindingItemID] = struct{}{}
		}
		for bindingID := range byBinding {
			record := byBinding[bindingID]
			if _, ok := seen[bindingID]; ok {
				continue
			}
			if record.State == ShortcutRootStateRemovedFinalDrain ||
				record.State == ShortcutRootStateRemovedReleasePending ||
				record.State == ShortcutRootStateRemovedCleanupBlocked ||
				record.State == ShortcutRootStateRemovedChildCleanupPending ||
				record.State == ShortcutRootStateSamePathReplacementWaiting {
				continue
			}
			record = plannedShortcutRootTransition(record,
				shortcutRootEventCompleteOmission,
				ShortcutRootStateRemovedFinalDrain,
				"",
			)
			record.ProtectedPaths = protectedPathsForShortcutRoot(record.RelativeLocalPath, record.ProtectedPaths)
			byBinding[bindingID] = record
			changed = true
		}
	}

	if markDuplicateShortcutTargets(byBinding) {
		changed = true
	}

	records := make([]ShortcutRootRecord, 0, len(byBinding))
	for bindingID := range byBinding {
		record := byBinding[bindingID]
		records = append(records, normalizeShortcutRootRecord(record))
	}
	slices.SortFunc(records, func(a, b ShortcutRootRecord) int {
		if a.RelativeLocalPath == b.RelativeLocalPath {
			return compareString(a.BindingItemID, b.BindingItemID)
		}
		return compareString(a.RelativeLocalPath, b.RelativeLocalPath)
	})
	return shortcutRootTopologyPlan{Records: records, Changed: changed}
}

func planShortcutRootDrainReleasePending(
	current []ShortcutRootRecord,
	ack ShortcutChildDrainAck,
) shortcutRootDrainAckPlan {
	records := make([]ShortcutRootRecord, 0, len(current))
	changed := false
	for i := range current {
		record := normalizeShortcutRootRecord(current[i])
		if record.BindingItemID != ack.BindingItemID {
			records = append(records, record)
			continue
		}
		if !shortcutRootStateAwaitsFinalDrainAck(record.State) {
			records = append(records, record)
			continue
		}
		next := plannedShortcutRootTransition(record,
			shortcutRootEventChildFinalDrainClean,
			ShortcutRootStateRemovedReleasePending,
			"",
		)
		records = append(records, next)
		changed = changed || !shortcutRootRecordsEqual(record, next)
	}
	slices.SortFunc(records, func(a, b ShortcutRootRecord) int {
		if a.RelativeLocalPath == b.RelativeLocalPath {
			return compareString(a.BindingItemID, b.BindingItemID)
		}
		return compareString(a.RelativeLocalPath, b.RelativeLocalPath)
	})
	return shortcutRootDrainAckPlan{Records: records, Changed: changed}
}

type shortcutRootReleaseCleanupPlan struct {
	Records []ShortcutRootRecord
	Changed bool
	Err     error
}

// planShortcutRootReleaseCleanup is the deterministic core for the parent
// release-cleanup phase. The engine shell owns filesystem removal; this helper
// only translates that outcome into the next durable shortcut-root records.
func planShortcutRootReleaseCleanup(
	record *ShortcutRootRecord,
	cleanupErr error,
) shortcutRootReleaseCleanupPlan {
	if record == nil {
		return shortcutRootReleaseCleanupPlan{}
	}
	normalized := normalizeShortcutRootRecord(*record)
	if !shortcutRootStateAwaitsReleaseCleanup(normalized.State) {
		return shortcutRootReleaseCleanupPlan{
			Records: []ShortcutRootRecord{normalized},
		}
	}
	if cleanupErr != nil {
		next := shortcutRootCleanupBlocked(normalized, cleanupErr)
		return shortcutRootReleaseCleanupPlan{
			Records: []ShortcutRootRecord{next},
			Changed: !shortcutRootRecordsEqual(normalized, next),
			Err:     cleanupErr,
		}
	}
	cleanupPending := shortcutRootChildCleanupPending(normalized)
	nextRecords := []ShortcutRootRecord{cleanupPending}
	if normalized.Waiting != nil {
		nextRecords = append(nextRecords, shortcutRootRecordFromReplacement(normalized.NamespaceID, *normalized.Waiting))
	}
	return shortcutRootReleaseCleanupPlan{
		Records: nextRecords,
		Changed: true,
	}
}

func planShortcutRootArtifactCleanupAck(
	current []ShortcutRootRecord,
	ack ShortcutChildArtifactCleanupAck,
) shortcutRootArtifactCleanupAckPlan {
	records := make([]ShortcutRootRecord, 0, len(current))
	changed := false
	for i := range current {
		record := normalizeShortcutRootRecord(current[i])
		if record.BindingItemID == ack.BindingItemID &&
			record.State == ShortcutRootStateRemovedChildCleanupPending {
			changed = true
			continue
		}
		records = append(records, record)
	}
	return shortcutRootArtifactCleanupAckPlan{Records: records, Changed: changed}
}

func markDuplicateShortcutTargets(records map[string]ShortcutRootRecord) bool {
	duplicateBindings := duplicateShortcutTargetDetails(records)
	return applyDuplicateShortcutTargetDetails(records, duplicateBindings)
}

type duplicateShortcutTargetKey struct {
	namespaceID string
	driveID     string
	itemID      string
}

func duplicateShortcutTargetDetails(records map[string]ShortcutRootRecord) map[string]string {
	byTarget := make(map[duplicateShortcutTargetKey][]ShortcutRootRecord)
	for bindingID := range records {
		record := normalizeShortcutRootRecord(records[bindingID])
		if !shortcutRootParticipatesInDuplicateTargetCheck(&record) {
			continue
		}
		key := duplicateShortcutTargetKey{
			namespaceID: record.NamespaceID,
			driveID:     record.RemoteDriveID.String(),
			itemID:      record.RemoteItemID,
		}
		byTarget[key] = append(byTarget[key], record)
	}

	duplicateBindings := make(map[string]string)
	for _, group := range byTarget {
		if len(group) <= 1 {
			continue
		}
		slices.SortFunc(group, func(a, b ShortcutRootRecord) int {
			if a.RelativeLocalPath == b.RelativeLocalPath {
				return compareString(a.BindingItemID, b.BindingItemID)
			}
			return compareString(a.RelativeLocalPath, b.RelativeLocalPath)
		})
		winner := group[0]
		for i := 1; i < len(group); i++ {
			duplicateBindings[group[i].BindingItemID] = fmt.Sprintf(
				"shortcut target already projected by %s at %s",
				winner.BindingItemID,
				winner.RelativeLocalPath,
			)
		}
	}
	return duplicateBindings
}

func applyDuplicateShortcutTargetDetails(
	records map[string]ShortcutRootRecord,
	duplicateBindings map[string]string,
) bool {
	changed := false
	for bindingID := range records {
		record := normalizeShortcutRootRecord(records[bindingID])
		detail, isDuplicate := duplicateBindings[bindingID]
		switch {
		case isDuplicate && record.State != ShortcutRootStateDuplicateTarget:
			next := plannedShortcutRootTransition(record,
				shortcutRootEventDuplicateTargetDetected,
				ShortcutRootStateDuplicateTarget,
				detail,
			)
			if !shortcutRootRecordsEqual(record, next) {
				records[bindingID] = next
				changed = true
			}
		case isDuplicate && record.BlockedDetail != detail:
			record.BlockedDetail = detail
			records[bindingID] = record
			changed = true
		case !isDuplicate && record.State == ShortcutRootStateDuplicateTarget:
			next := plannedShortcutRootTransition(record,
				shortcutRootEventDuplicateTargetResolved,
				ShortcutRootStateActive,
				"",
			)
			if !shortcutRootRecordsEqual(record, next) {
				records[bindingID] = next
				changed = true
			}
		}
	}
	return changed
}

func shortcutRootParticipatesInDuplicateTargetCheck(record *ShortcutRootRecord) bool {
	if record == nil {
		return false
	}
	normalized := normalizeShortcutRootRecord(*record)
	if normalized.RemoteDriveID.IsZero() || normalized.RemoteItemID == "" {
		return false
	}
	switch normalized.State {
	case ShortcutRootStateActive,
		ShortcutRootStateDuplicateTarget:
		return true
	case "",
		ShortcutRootStateTargetUnavailable,
		ShortcutRootStateLocalRootUnavailable,
		ShortcutRootStateBlockedPath,
		ShortcutRootStateRenameAmbiguous,
		ShortcutRootStateAliasMutationBlocked,
		ShortcutRootStateRemovedFinalDrain,
		ShortcutRootStateRemovedReleasePending,
		ShortcutRootStateRemovedCleanupBlocked,
		ShortcutRootStateRemovedChildCleanupPending,
		ShortcutRootStateSamePathReplacementWaiting:
		return false
	default:
		return false
	}
}

//nolint:gocritic // ShortcutRootRecord is an immutable planner value at this boundary.
func upsertShortcutRootRecord(records map[string]ShortcutRootRecord, next ShortcutRootRecord) bool {
	next = normalizeShortcutRootRecord(next)
	current, found := records[next.BindingItemID]
	if found && shortcutRootRecordsEqual(current, next) {
		return false
	}
	records[next.BindingItemID] = next
	return true
}

func shortcutRootRecordFromUpsert(namespaceID string, fact shortcutBindingUpsert) ShortcutRootRecord {
	return normalizeShortcutRootRecord(ShortcutRootRecord{
		NamespaceID:       namespaceID,
		BindingItemID:     fact.BindingItemID,
		RelativeLocalPath: fact.RelativeLocalPath,
		LocalAlias:        fact.LocalAlias,
		RemoteDriveID:     driveid.New(fact.RemoteDriveID),
		RemoteItemID:      fact.RemoteItemID,
		RemoteIsFolder:    fact.RemoteIsFolder,
		State:             ShortcutRootStateActive,
		ProtectedPaths:    []string{fact.RelativeLocalPath},
	})
}

func shortcutRootRecordFromUnavailable(namespaceID string, fact shortcutBindingUnavailable) ShortcutRootRecord {
	return normalizeShortcutRootRecord(ShortcutRootRecord{
		NamespaceID:       namespaceID,
		BindingItemID:     fact.BindingItemID,
		RelativeLocalPath: fact.RelativeLocalPath,
		LocalAlias:        fact.LocalAlias,
		RemoteDriveID:     driveid.New(fact.RemoteDriveID),
		RemoteItemID:      fact.RemoteItemID,
		RemoteIsFolder:    fact.RemoteIsFolder,
		State:             ShortcutRootStateTargetUnavailable,
		ProtectedPaths:    []string{fact.RelativeLocalPath},
		BlockedDetail:     fact.Reason,
	})
}

func samePathRetiringShortcutRoot(
	records map[string]ShortcutRootRecord,
	relativeLocalPath string,
	nextBindingID string,
) (ShortcutRootRecord, bool) {
	if relativeLocalPath == "" {
		return ShortcutRootRecord{}, false
	}
	for bindingID := range records {
		record := records[bindingID]
		if bindingID == nextBindingID || record.RelativeLocalPath != relativeLocalPath {
			continue
		}
		if record.State == ShortcutRootStateRemovedFinalDrain ||
			record.State == ShortcutRootStateRemovedReleasePending ||
			record.State == ShortcutRootStateRemovedCleanupBlocked ||
			record.State == ShortcutRootStateSamePathReplacementWaiting {
			return record, true
		}
	}
	return ShortcutRootRecord{}, false
}

func activeProtectedShortcutRootForPath(
	records map[string]ShortcutRootRecord,
	relativeLocalPath string,
	nextBindingID string,
) (ShortcutRootRecord, bool) {
	if relativeLocalPath == "" {
		return ShortcutRootRecord{}, false
	}
	normalizedPath := normalizedProtectedRootPath(relativeLocalPath)
	for bindingID := range records {
		record := normalizeShortcutRootRecord(records[bindingID])
		if bindingID == nextBindingID || !shortcutRootStateKeepsProtectedPaths(record.State) {
			continue
		}
		if record.State == ShortcutRootStateRemovedFinalDrain ||
			record.State == ShortcutRootStateRemovedReleasePending ||
			record.State == ShortcutRootStateRemovedCleanupBlocked ||
			record.State == ShortcutRootStateSamePathReplacementWaiting {
			continue
		}
		for _, protectedPath := range record.ProtectedPaths {
			if normalizedProtectedRootPath(protectedPath) == normalizedPath {
				return record, true
			}
		}
	}
	return ShortcutRootRecord{}, false
}

func shortcutRootRecordFromReplacement(namespaceID string, replacement ShortcutRootReplacement) ShortcutRootRecord {
	return normalizeShortcutRootRecord(ShortcutRootRecord{
		NamespaceID:       namespaceID,
		BindingItemID:     replacement.BindingItemID,
		RelativeLocalPath: replacement.RelativeLocalPath,
		LocalAlias:        replacement.LocalAlias,
		RemoteDriveID:     replacement.RemoteDriveID,
		RemoteItemID:      replacement.RemoteItemID,
		RemoteIsFolder:    replacement.RemoteIsFolder,
		State:             ShortcutRootStateActive,
		ProtectedPaths:    []string{replacement.RelativeLocalPath},
	})
}

func compareString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
