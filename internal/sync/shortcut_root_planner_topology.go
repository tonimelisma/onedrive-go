package sync

import (
	"fmt"
	"slices"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

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
	changed = applyShortcutRootRemoteDeletes(byBinding, batch.Deletes) || changed
	changed = applyShortcutRootRemoteUnavailable(byBinding, batch.NamespaceID, batch.Unavailable) || changed
	changed = applyShortcutRootRemoteUpserts(byBinding, batch.NamespaceID, batch.Upserts) || changed
	changed = applyShortcutRootCompleteOmission(byBinding, batch) || changed
	changed = markDuplicateShortcutTargets(byBinding) || changed

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

func applyShortcutRootRemoteDeletes(
	byBinding map[string]ShortcutRootRecord,
	deletes []shortcutBindingDelete,
) bool {
	changed := false
	for i := range deletes {
		record, ok := byBinding[deletes[i].BindingItemID]
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
	return changed
}

func applyShortcutRootRemoteUnavailable(
	byBinding map[string]ShortcutRootRecord,
	namespaceID string,
	unavailable []shortcutBindingUnavailable,
) bool {
	changed := false
	for i := range unavailable {
		fact := unavailable[i]
		next := shortcutRootRecordFromUnavailable(namespaceID, fact)
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
	return changed
}

func applyShortcutRootRemoteUpserts(
	byBinding map[string]ShortcutRootRecord,
	namespaceID string,
	upserts []shortcutBindingUpsert,
) bool {
	changed := false
	for i := range upserts {
		next := shortcutRootRecordFromUpsert(namespaceID, upserts[i])
		if applyShortcutRootSamePathReplacement(byBinding, &next) {
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
		if existing, ok := byBinding[upserts[i].BindingItemID]; ok {
			next = planShortcutRootRemoteUpsertForExisting(existing, next)
		}
		changed = upsertShortcutRootRecord(byBinding, next) || changed
	}
	return changed
}

func applyShortcutRootSamePathReplacement(
	byBinding map[string]ShortcutRootRecord,
	next *ShortcutRootRecord,
) bool {
	if next == nil {
		return false
	}
	owner, found := samePathRetiringShortcutRoot(byBinding, next.RelativeLocalPath, next.BindingItemID)
	if !found {
		return false
	}
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
	return true
}

//nolint:gocritic // ShortcutRootRecord is an immutable planner value at this boundary.
func planShortcutRootRemoteUpsertForExisting(
	existing ShortcutRootRecord,
	next ShortcutRootRecord,
) ShortcutRootRecord {
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
	return next
}

func applyShortcutRootCompleteOmission(
	byBinding map[string]ShortcutRootRecord,
	batch shortcutTopologyBatch,
) bool {
	if batch.Kind != shortcutTopologyObservationComplete {
		return false
	}
	seen := shortcutRootCompleteBatchSeenBindings(batch)
	changed := false
	for bindingID := range byBinding {
		record := byBinding[bindingID]
		if _, ok := seen[bindingID]; ok || shortcutRootCompleteOmissionKeepsState(record.State) {
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
	return changed
}

func shortcutRootCompleteBatchSeenBindings(batch shortcutTopologyBatch) map[string]struct{} {
	seen := make(map[string]struct{}, len(batch.Upserts)+len(batch.Unavailable))
	for i := range batch.Upserts {
		seen[batch.Upserts[i].BindingItemID] = struct{}{}
	}
	for i := range batch.Unavailable {
		seen[batch.Unavailable[i].BindingItemID] = struct{}{}
	}
	return seen
}

func shortcutRootCompleteOmissionKeepsState(state ShortcutRootState) bool {
	switch state {
	case ShortcutRootStateRemovedFinalDrain,
		ShortcutRootStateRemovedReleasePending,
		ShortcutRootStateRemovedCleanupBlocked,
		ShortcutRootStateRemovedChildCleanupPending,
		ShortcutRootStateSamePathReplacementWaiting:
		return true
	case "",
		ShortcutRootStateActive,
		ShortcutRootStateTargetUnavailable,
		ShortcutRootStateLocalRootUnavailable,
		ShortcutRootStateBlockedPath,
		ShortcutRootStateRenameAmbiguous,
		ShortcutRootStateAliasMutationBlocked,
		ShortcutRootStateDuplicateTarget:
		return false
	default:
		return false
	}
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
