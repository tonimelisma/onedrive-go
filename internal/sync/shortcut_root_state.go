package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path"
	"slices"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// ShortcutRootState is the parent-engine-owned lifecycle state for a shortcut
// alias root in the parent sync namespace. It is not child content retry state.
type ShortcutRootState string

const (
	ShortcutRootStateActive                     ShortcutRootState = "active"
	ShortcutRootStateTargetUnavailable          ShortcutRootState = "target_unavailable"
	ShortcutRootStateBlockedPath                ShortcutRootState = "blocked_path"
	ShortcutRootStateRenameAmbiguous            ShortcutRootState = "rename_ambiguous"
	ShortcutRootStateAliasMutationBlocked       ShortcutRootState = "alias_mutation_blocked"
	ShortcutRootStateRemovedFinalDrain          ShortcutRootState = "removed_final_drain"
	ShortcutRootStateRemovedCleanupBlocked      ShortcutRootState = "removed_cleanup_blocked"
	ShortcutRootStateSamePathReplacementWaiting ShortcutRootState = "same_path_replacement_waiting"
)

// ShortcutRootRecord is the persisted parent namespace truth for one shortcut
// placeholder observed by the parent engine.
type ShortcutRootRecord struct {
	NamespaceID       string
	BindingItemID     string
	RelativeLocalPath string
	LocalAlias        string
	RemoteDriveID     driveid.ID
	RemoteItemID      string
	RemoteIsFolder    bool
	State             ShortcutRootState
	ProtectedPaths    []string
	BlockedDetail     string
	LocalRootIdentity *synctree.FileIdentity
	Waiting           *ShortcutRootReplacement
}

// ShortcutRootReplacement stores a new shortcut binding that is waiting behind
// an older retiring binding at the same parent-local path.
type ShortcutRootReplacement struct {
	BindingItemID     string
	RelativeLocalPath string
	LocalAlias        string
	RemoteDriveID     driveid.ID
	RemoteItemID      string
	RemoteIsFolder    bool
}

type shortcutRootTopologyPlan struct {
	Records []ShortcutRootRecord
	Changed bool
}

type shortcutRootDrainAckPlan struct {
	Records []ShortcutRootRecord
	Changed bool
}

func localFilterWithPersistedShortcutRoots(
	ctx context.Context,
	store *SyncStore,
	filter LocalFilterConfig,
	namespaceID string,
) (LocalFilterConfig, error) {
	if store == nil || namespaceID == "" {
		return filter, nil
	}
	records, err := store.ListShortcutRoots(ctx)
	if err != nil {
		return filter, err
	}
	filter.ManagedRoots = mergeManagedRootReservations(
		filter.ManagedRoots,
		managedRootReservationsForShortcutRoots(records, namespaceID),
	)
	return filter, nil
}

func managedRootReservationsForShortcutRoots(records []ShortcutRootRecord, namespaceID string) []ManagedRootReservation {
	reservations := make([]ManagedRootReservation, 0)
	for i := range records {
		record := normalizeShortcutRootRecord(records[i])
		if record.NamespaceID != "" && record.NamespaceID != namespaceID {
			continue
		}
		if !shortcutRootStateKeepsProtectedPaths(record.State) {
			continue
		}
		paths := protectedPathsForShortcutRoot(record.RelativeLocalPath, record.ProtectedPaths)
		for _, protectedPath := range paths {
			reservation := ManagedRootReservation{
				Path:      protectedPath,
				MountID:   record.BindingItemID,
				BindingID: record.BindingItemID,
			}
			if record.LocalRootIdentity != nil {
				reservation.Device = record.LocalRootIdentity.Device
				reservation.Inode = record.LocalRootIdentity.Inode
				reservation.HasIdentity = true
			}
			reservations = append(reservations, reservation)
		}
	}
	return reservations
}

func shortcutRootStateKeepsProtectedPaths(state ShortcutRootState) bool {
	switch state {
	case "",
		ShortcutRootStateActive,
		ShortcutRootStateTargetUnavailable,
		ShortcutRootStateBlockedPath,
		ShortcutRootStateRenameAmbiguous,
		ShortcutRootStateAliasMutationBlocked,
		ShortcutRootStateRemovedFinalDrain,
		ShortcutRootStateRemovedCleanupBlocked,
		ShortcutRootStateSamePathReplacementWaiting:
		return true
	default:
		return false
	}
}

func mergeManagedRootReservations(current []ManagedRootReservation, persisted []ManagedRootReservation) []ManagedRootReservation {
	if len(persisted) == 0 {
		return current
	}
	merged := append([]ManagedRootReservation(nil), current...)
	for i := range persisted {
		next := persisted[i]
		next.Path = normalizedManagedRootPath(next.Path)
		if next.Path == "" || next.BindingID == "" {
			continue
		}
		replaced := false
		for j := range merged {
			if normalizedManagedRootPath(merged[j].Path) != next.Path {
				continue
			}
			if merged[j].BindingID != "" && merged[j].BindingID != next.BindingID {
				continue
			}
			if merged[j].MountID == "" {
				merged[j].MountID = next.MountID
			}
			if merged[j].BindingID == "" {
				merged[j].BindingID = next.BindingID
			}
			if !merged[j].HasIdentity && next.HasIdentity {
				merged[j].Device = next.Device
				merged[j].Inode = next.Inode
				merged[j].HasIdentity = true
			}
			replaced = true
			break
		}
		if !replaced {
			merged = append(merged, next)
		}
	}
	return merged
}

//nolint:funlen,gocyclo,gocritic // Topology planning keeps the state transition table in one deterministic pass.
func planShortcutRootTopology(
	current []ShortcutRootRecord,
	batch ShortcutTopologyBatch,
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
			next.State = plannedShortcutRootTransition(existing,
				shortcutRootEventRemoteUnavailable,
				ShortcutRootStateTargetUnavailable,
				fact.Reason,
			).State
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

	if batch.Kind == ShortcutTopologyObservationComplete {
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

func shortcutRootRecordFromUpsert(namespaceID string, fact ShortcutBindingUpsert) ShortcutRootRecord {
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

func shortcutRootRecordFromUnavailable(namespaceID string, fact ShortcutBindingUnavailable) ShortcutRootRecord {
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
	normalizedPath := normalizedManagedRootPath(relativeLocalPath)
	for bindingID := range records {
		record := normalizeShortcutRootRecord(records[bindingID])
		if bindingID == nextBindingID || !shortcutRootStateKeepsProtectedPaths(record.State) {
			continue
		}
		if record.State == ShortcutRootStateRemovedFinalDrain ||
			record.State == ShortcutRootStateRemovedCleanupBlocked ||
			record.State == ShortcutRootStateSamePathReplacementWaiting {
			continue
		}
		for _, protectedPath := range record.ProtectedPaths {
			if normalizedManagedRootPath(protectedPath) == normalizedPath {
				return record, true
			}
		}
	}
	return ShortcutRootRecord{}, false
}

//nolint:gocritic // Return-by-value normalization keeps planner mutations explicit.
func normalizeShortcutRootRecord(record ShortcutRootRecord) ShortcutRootRecord {
	if record.LocalAlias == "" && record.RelativeLocalPath != "" {
		record.LocalAlias = path.Base(record.RelativeLocalPath)
	}
	record.ProtectedPaths = protectedPathsForShortcutRoot(record.RelativeLocalPath, record.ProtectedPaths)
	if record.State == "" {
		record.State = ShortcutRootStateActive
	}
	if record.Waiting != nil && record.Waiting.BindingItemID == "" {
		record.Waiting = nil
	}
	return record
}

func protectedPathsForShortcutRoot(relativeLocalPath string, paths []string) []string {
	unique := make(map[string]struct{}, len(paths)+1)
	result := make([]string, 0, len(paths)+1)
	add := func(value string) {
		normalized := normalizedManagedRootPath(value)
		if normalized == "" {
			return
		}
		if _, exists := unique[normalized]; exists {
			return
		}
		unique[normalized] = struct{}{}
		result = append(result, normalized)
	}
	add(relativeLocalPath)
	for _, protectedPath := range paths {
		add(protectedPath)
	}
	return result
}

//nolint:gocritic // Equality compares normalized value snapshots from the planner.
func shortcutRootRecordsEqual(a, b ShortcutRootRecord) bool {
	a = normalizeShortcutRootRecord(a)
	b = normalizeShortcutRootRecord(b)
	if a.NamespaceID != b.NamespaceID ||
		a.BindingItemID != b.BindingItemID ||
		a.RelativeLocalPath != b.RelativeLocalPath ||
		a.LocalAlias != b.LocalAlias ||
		a.RemoteDriveID.String() != b.RemoteDriveID.String() ||
		a.RemoteItemID != b.RemoteItemID ||
		a.RemoteIsFolder != b.RemoteIsFolder ||
		a.State != b.State ||
		a.BlockedDetail != b.BlockedDetail ||
		!slices.Equal(a.ProtectedPaths, b.ProtectedPaths) {
		return false
	}
	if !shortcutRootIdentitiesEqual(a.LocalRootIdentity, b.LocalRootIdentity) {
		return false
	}
	return shortcutRootReplacementsEqual(a.Waiting, b.Waiting)
}

func shortcutRootIdentitiesEqual(a, b *synctree.FileIdentity) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return synctree.SameIdentity(*a, *b)
}

func shortcutRootReplacementsEqual(a, b *ShortcutRootReplacement) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.BindingItemID == b.BindingItemID &&
		a.RelativeLocalPath == b.RelativeLocalPath &&
		a.LocalAlias == b.LocalAlias &&
		a.RemoteDriveID.String() == b.RemoteDriveID.String() &&
		a.RemoteItemID == b.RemoteItemID &&
		a.RemoteIsFolder == b.RemoteIsFolder
}

func cloneShortcutRootReplacement(replacement *ShortcutRootReplacement) *ShortcutRootReplacement {
	if replacement == nil {
		return nil
	}
	next := *replacement
	return &next
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

func planShortcutRootDrainAck(
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
		switch {
		case record.State == ShortcutRootStateSamePathReplacementWaiting && record.Waiting != nil:
			if err := validateShortcutRootTransition(
				record.State,
				shortcutRootEventWaitingReplacementPromote,
				ShortcutRootStateActive,
			); err != nil {
				records = append(records, plannedShortcutRootTransition(record,
					shortcutRootEventProjectionCleanupFailed,
					ShortcutRootStateRemovedCleanupBlocked,
					err.Error(),
				))
				changed = true
				continue
			}
			records = append(records, shortcutRootRecordFromReplacement(record.NamespaceID, *record.Waiting))
			changed = true
		case record.State == ShortcutRootStateRemovedFinalDrain ||
			record.State == ShortcutRootStateRemovedCleanupBlocked:
			changed = true
		default:
			records = append(records, record)
		}
	}
	slices.SortFunc(records, func(a, b ShortcutRootRecord) int {
		if a.RelativeLocalPath == b.RelativeLocalPath {
			return compareString(a.BindingItemID, b.BindingItemID)
		}
		return compareString(a.RelativeLocalPath, b.RelativeLocalPath)
	})
	return shortcutRootDrainAckPlan{Records: records, Changed: changed}
}

func shortcutChildTopologyFromRoots(namespaceID string, roots []ShortcutRootRecord) ShortcutChildTopologySnapshot {
	snapshot := ShortcutChildTopologySnapshot{
		NamespaceID: namespaceID,
		Children:    make([]ShortcutChildTopology, 0, len(roots)),
	}
	for i := range roots {
		root := normalizeShortcutRootRecord(roots[i])
		if root.NamespaceID != "" && root.NamespaceID != namespaceID {
			continue
		}
		child := ShortcutChildTopology{
			BindingItemID:     root.BindingItemID,
			RelativeLocalPath: root.RelativeLocalPath,
			LocalAlias:        root.LocalAlias,
			RemoteDriveID:     root.RemoteDriveID.String(),
			RemoteItemID:      root.RemoteItemID,
			RemoteIsFolder:    root.RemoteIsFolder,
			State:             shortcutChildStateForRoot(root.State),
			BlockedDetail:     root.BlockedDetail,
			ProtectedPaths:    protectedPathsForShortcutRoot(root.RelativeLocalPath, root.ProtectedPaths),
		}
		if root.Waiting != nil {
			waiting := shortcutChildTopologyFromReplacement(*root.Waiting)
			child.Waiting = &waiting
		}
		snapshot.Children = append(snapshot.Children, child)
	}
	return snapshot
}

func shortcutChildTopologyFromReplacement(replacement ShortcutRootReplacement) ShortcutChildTopology {
	return ShortcutChildTopology{
		BindingItemID:     replacement.BindingItemID,
		RelativeLocalPath: replacement.RelativeLocalPath,
		LocalAlias:        replacement.LocalAlias,
		RemoteDriveID:     replacement.RemoteDriveID.String(),
		RemoteItemID:      replacement.RemoteItemID,
		RemoteIsFolder:    replacement.RemoteIsFolder,
		State:             ShortcutChildWaitingReplacement,
		ProtectedPaths:    []string{replacement.RelativeLocalPath},
	}
}

func shortcutChildStateForRoot(state ShortcutRootState) ShortcutChildTopologyState {
	switch state {
	case "", ShortcutRootStateActive:
		return ShortcutChildDesired
	case ShortcutRootStateRemovedFinalDrain:
		return ShortcutChildRetiring
	case ShortcutRootStateSamePathReplacementWaiting:
		return ShortcutChildRetiring
	case ShortcutRootStateTargetUnavailable,
		ShortcutRootStateBlockedPath,
		ShortcutRootStateRenameAmbiguous,
		ShortcutRootStateAliasMutationBlocked,
		ShortcutRootStateRemovedCleanupBlocked:
		return ShortcutChildBlocked
	default:
		return ShortcutChildBlocked
	}
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

// ApplyShortcutTopology persists parent-owned shortcut-root state. Callers run
// this before committing remote observation progress so topology facts replay if
// the parent namespace state cannot be durably accepted.
//
//nolint:gocritic // ShortcutTopologyBatch is the observer boundary value type.
func (m *SyncStore) ApplyShortcutTopology(ctx context.Context, batch ShortcutTopologyBatch) (bool, error) {
	if m == nil || !batch.ShouldApply() {
		return false, nil
	}
	current, err := m.ListShortcutRoots(ctx)
	if err != nil {
		return false, err
	}
	plan := planShortcutRootTopology(current, batch)
	if !plan.Changed {
		return false, nil
	}
	if err := m.ReplaceShortcutRoots(ctx, plan.Records); err != nil {
		return false, err
	}
	return true, nil
}

func (m *SyncStore) AcknowledgeShortcutChildFinalDrain(ctx context.Context, ack ShortcutChildDrainAck) (bool, error) {
	if m == nil || ack.BindingItemID == "" {
		return false, nil
	}
	current, err := m.ListShortcutRoots(ctx)
	if err != nil {
		return false, err
	}
	plan := planShortcutRootDrainAck(current, ack)
	if !plan.Changed {
		return false, nil
	}
	if err := m.ReplaceShortcutRoots(ctx, plan.Records); err != nil {
		return false, err
	}
	return true, nil
}

func (m *SyncStore) ShortcutChildTopology(ctx context.Context, namespaceID string) (ShortcutChildTopologySnapshot, error) {
	if m == nil {
		return ShortcutChildTopologySnapshot{NamespaceID: namespaceID}, nil
	}
	records, err := m.ListShortcutRoots(ctx)
	if err != nil {
		return ShortcutChildTopologySnapshot{}, err
	}
	return shortcutChildTopologyFromRoots(namespaceID, records), nil
}

// ListShortcutRoots returns parent-engine-owned shortcut root state.
func (m *SyncStore) ListShortcutRoots(ctx context.Context) ([]ShortcutRootRecord, error) {
	return queryShortcutRootRecords(ctx, m.db, "sync: querying shortcut_roots", "sync: iterating shortcut_roots")
}

func queryShortcutRootRecords(
	ctx context.Context,
	db *sql.DB,
	queryContext string,
	iterContext string,
) ([]ShortcutRootRecord, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT namespace_id, binding_item_id, relative_local_path, local_alias,
		       remote_drive_id, remote_item_id, remote_is_folder, state,
		       protected_paths_json, blocked_detail, local_root_device,
		       local_root_inode, local_root_has_identity, waiting_binding_item_id,
		       waiting_relative_local_path, waiting_local_alias,
		       waiting_remote_drive_id, waiting_remote_item_id,
		       waiting_remote_is_folder
		FROM shortcut_roots
		ORDER BY relative_local_path, binding_item_id`)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", queryContext, err)
	}
	defer rows.Close()

	records := make([]ShortcutRootRecord, 0)
	for rows.Next() {
		record, scanErr := scanShortcutRootRecord(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", iterContext, err)
	}
	return records, nil
}

// ReplaceShortcutRoots atomically replaces the parent shortcut-root table.
func (m *SyncStore) ReplaceShortcutRoots(ctx context.Context, records []ShortcutRootRecord) (err error) {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync: beginning shortcut_roots replacement: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback shortcut_roots replacement")
	}()

	if _, err = tx.ExecContext(ctx, `DELETE FROM shortcut_roots`); err != nil {
		return fmt.Errorf("sync: clearing shortcut_roots: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO shortcut_roots (
			namespace_id, binding_item_id, relative_local_path, local_alias,
			remote_drive_id, remote_item_id, remote_is_folder, state,
			protected_paths_json, blocked_detail, local_root_device,
			local_root_inode, local_root_has_identity, waiting_binding_item_id,
			waiting_relative_local_path, waiting_local_alias,
			waiting_remote_drive_id, waiting_remote_item_id,
			waiting_remote_is_folder
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("sync: preparing shortcut_roots upsert: %w", err)
	}
	defer stmt.Close()

	for i := range records {
		record := normalizeShortcutRootRecord(records[i])
		protectedPaths, marshalErr := json.Marshal(record.ProtectedPaths)
		if marshalErr != nil {
			return fmt.Errorf("sync: encoding protected paths for shortcut root %s: %w", record.BindingItemID, marshalErr)
		}
		device, inode, hasIdentity := shortcutRootIdentitySQL(record.LocalRootIdentity)
		waiting := record.Waiting
		if waiting == nil {
			waiting = &ShortcutRootReplacement{}
		}
		if _, err = stmt.ExecContext(ctx,
			record.NamespaceID,
			record.BindingItemID,
			record.RelativeLocalPath,
			record.LocalAlias,
			record.RemoteDriveID.String(),
			record.RemoteItemID,
			boolInt(record.RemoteIsFolder),
			string(record.State),
			string(protectedPaths),
			record.BlockedDetail,
			device,
			inode,
			boolInt(hasIdentity),
			waiting.BindingItemID,
			waiting.RelativeLocalPath,
			waiting.LocalAlias,
			waiting.RemoteDriveID.String(),
			waiting.RemoteItemID,
			boolInt(waiting.RemoteIsFolder),
		); err != nil {
			return fmt.Errorf("sync: inserting shortcut root %s: %w", record.BindingItemID, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing shortcut_roots replacement: %w", err)
	}
	return nil
}

type shortcutRootScanner interface {
	Scan(dest ...any) error
}

func scanShortcutRootRecord(row shortcutRootScanner) (ShortcutRootRecord, error) {
	var (
		remoteIsFolder        int
		protectedPathsJSON    string
		localRootDevice       uint64
		localRootInode        uint64
		localRootHasIdentity  int
		waitingRemoteIsFolder int
		state                 string
		record                ShortcutRootRecord
		waiting               ShortcutRootReplacement
	)
	if err := row.Scan(
		&record.NamespaceID,
		&record.BindingItemID,
		&record.RelativeLocalPath,
		&record.LocalAlias,
		&record.RemoteDriveID,
		&record.RemoteItemID,
		&remoteIsFolder,
		&state,
		&protectedPathsJSON,
		&record.BlockedDetail,
		&localRootDevice,
		&localRootInode,
		&localRootHasIdentity,
		&waiting.BindingItemID,
		&waiting.RelativeLocalPath,
		&waiting.LocalAlias,
		&waiting.RemoteDriveID,
		&waiting.RemoteItemID,
		&waitingRemoteIsFolder,
	); err != nil {
		return ShortcutRootRecord{}, fmt.Errorf("sync: scanning shortcut_roots row: %w", err)
	}
	if err := json.Unmarshal([]byte(protectedPathsJSON), &record.ProtectedPaths); err != nil {
		return ShortcutRootRecord{}, fmt.Errorf("sync: decoding shortcut root protected paths for %s: %w", record.BindingItemID, err)
	}
	record.RemoteIsFolder = remoteIsFolder != 0
	record.State = ShortcutRootState(state)
	if localRootHasIdentity != 0 {
		record.LocalRootIdentity = &synctree.FileIdentity{Device: localRootDevice, Inode: localRootInode}
	}
	waiting.RemoteIsFolder = waitingRemoteIsFolder != 0
	if waiting.BindingItemID != "" {
		record.Waiting = &waiting
	}
	return normalizeShortcutRootRecord(record), nil
}

func shortcutRootIdentitySQL(identity *synctree.FileIdentity) (uint64, uint64, bool) {
	if identity == nil {
		return 0, 0, false
	}
	return identity.Device, identity.Inode, true
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
