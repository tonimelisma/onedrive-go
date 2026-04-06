package synctypes

// ScopeStateRecord is the durable store-owned projection of the current sync
// scope. The engine computes the next record, but the store owns persisting it
// and updating filtered remote-state rows atomically.
type ScopeStateRecord struct {
	Generation            int64
	EffectiveSnapshotJSON string
	ObservationPlanHash   string
	ObservationMode       ScopeObservationMode
	WebsocketEnabled      bool
	PendingReentry        bool
	LastReconcileKind     ScopeReconcileKind
	UpdatedAt             int64
}

// ScopeStateApplyRequest is one atomic scope-state transition. The store uses
// the effective snapshot to re-evaluate remote_state rows and the metadata
// fields to publish the current engine-owned observation plan.
type ScopeStateApplyRequest struct {
	State ScopeStateRecord
}
