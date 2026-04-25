package sync

import (
	"context"
	"path"
	"slices"
)

type ShortcutTopologyObservationKind string

const (
	ShortcutTopologyObservationIncremental ShortcutTopologyObservationKind = "incremental"
	ShortcutTopologyObservationComplete    ShortcutTopologyObservationKind = "complete"
)

type ShortcutTopologyBatch struct {
	NamespaceID string
	Kind        ShortcutTopologyObservationKind
	Upserts     []ShortcutBindingUpsert
	Deletes     []ShortcutBindingDelete
	Unavailable []ShortcutBindingUnavailable
}

type ShortcutBindingUpsert struct {
	BindingItemID     string
	RelativeLocalPath string
	LocalAlias        string
	RemoteDriveID     string
	RemoteItemID      string
	RemoteIsFolder    bool
	Complete          bool
}

type ShortcutBindingDelete struct {
	BindingItemID string
}

type ShortcutBindingUnavailable struct {
	BindingItemID     string
	RelativeLocalPath string
	LocalAlias        string
	RemoteDriveID     string
	RemoteItemID      string
	RemoteIsFolder    bool
	Reason            string
}

type ShortcutTopologyHandler func(context.Context, ShortcutTopologyBatch) error

func (b ShortcutTopologyBatch) HasFacts() bool {
	return len(b.Upserts) > 0 || len(b.Deletes) > 0 || len(b.Unavailable) > 0
}

func (b ShortcutTopologyBatch) ShouldApply() bool {
	return b.HasFacts() || b.Kind == ShortcutTopologyObservationComplete
}

func (b *ShortcutTopologyBatch) appendUpsert(fact ShortcutBindingUpsert) {
	if fact.BindingItemID == "" {
		return
	}
	for i := range b.Upserts {
		if b.Upserts[i].BindingItemID == fact.BindingItemID {
			b.Upserts[i] = fact
			return
		}
	}
	b.removeUnavailable(fact.BindingItemID)
	b.removeDelete(fact.BindingItemID)
	b.Upserts = append(b.Upserts, fact)
}

func (b *ShortcutTopologyBatch) appendDelete(fact ShortcutBindingDelete) {
	if fact.BindingItemID == "" {
		return
	}
	b.removeUpsert(fact.BindingItemID)
	b.removeUnavailable(fact.BindingItemID)
	if slices.ContainsFunc(b.Deletes, func(existing ShortcutBindingDelete) bool {
		return existing.BindingItemID == fact.BindingItemID
	}) {
		return
	}
	b.Deletes = append(b.Deletes, fact)
}

func (b *ShortcutTopologyBatch) appendUnavailable(fact ShortcutBindingUnavailable) {
	if fact.BindingItemID == "" {
		return
	}
	for i := range b.Unavailable {
		if b.Unavailable[i].BindingItemID == fact.BindingItemID {
			b.Unavailable[i] = fact
			return
		}
	}
	b.removeUpsert(fact.BindingItemID)
	b.removeDelete(fact.BindingItemID)
	b.Unavailable = append(b.Unavailable, fact)
}

func (b *ShortcutTopologyBatch) removeUpsert(bindingItemID string) {
	b.Upserts = slices.DeleteFunc(b.Upserts, func(existing ShortcutBindingUpsert) bool {
		return existing.BindingItemID == bindingItemID
	})
}

func (b *ShortcutTopologyBatch) removeDelete(bindingItemID string) {
	b.Deletes = slices.DeleteFunc(b.Deletes, func(existing ShortcutBindingDelete) bool {
		return existing.BindingItemID == bindingItemID
	})
}

func (b *ShortcutTopologyBatch) removeUnavailable(bindingItemID string) {
	b.Unavailable = slices.DeleteFunc(b.Unavailable, func(existing ShortcutBindingUnavailable) bool {
		return existing.BindingItemID == bindingItemID
	})
}

func managedRootReservationByBinding(reservations []ManagedRootReservation) map[string]ManagedRootReservation {
	byBinding := make(map[string]ManagedRootReservation, len(reservations))
	for i := range reservations {
		reservation := reservations[i]
		if reservation.BindingID == "" {
			continue
		}
		byBinding[reservation.BindingID] = reservation
	}
	return byBinding
}

func managedRootPrimaryName(relPath string) string {
	if relPath == "" {
		return ""
	}
	return path.Base(relPath)
}
