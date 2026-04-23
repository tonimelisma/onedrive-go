package sync

// PermissionOutcomeKind is the engine-facing policy decision produced from one
// action completion plus permission evidence.
type PermissionOutcomeKind int

const (
	permissionOutcomeNone PermissionOutcomeKind = iota
	permissionOutcomeRecordFileFailure
	permissionOutcomeActivateBoundaryScope
	permissionOutcomeActivateDerivedScope
)

// PermissionOutcome is the policy-layer output consumed by the engine apply
// path. Matched=false means the engine should fall back to generic result
// persistence.
type PermissionOutcome struct {
	Matched          bool
	Kind             PermissionOutcomeKind
	RetryWorkFailure *RetryWorkFailure
	ScopeKey         ScopeKey
	BoundaryPath     string
	TriggerPath      string
}

// ConditionKey returns the stable cross-authority condition family for this
// policy result. The outcome owns this mapping so apply/logging code does not
// need to reconstruct outcome semantics from partially overlapping fields.
func (o PermissionOutcome) ConditionKey() ConditionKey {
	if !o.ScopeKey.IsZero() {
		return ConditionKeyForStoredCondition(o.ScopeKey.ConditionType(), o.ScopeKey)
	}

	if o.RetryWorkFailure != nil {
		return ConditionKeyForStoredCondition(
			o.RetryWorkFailure.ConditionType,
			o.RetryWorkFailure.ScopeKey,
		)
	}

	return ""
}

func (o PermissionOutcome) IsFileFailure() bool {
	return o.Kind == permissionOutcomeRecordFileFailure
}

func (o PermissionOutcome) IsBoundaryFailure() bool {
	return o.Kind == permissionOutcomeActivateBoundaryScope ||
		o.Kind == permissionOutcomeActivateDerivedScope
}

func (o PermissionOutcome) ActivatesDerivedScope() bool {
	return o.Kind == permissionOutcomeActivateDerivedScope
}

func DecidePermissionOutcome(
	r *ActionCompletion,
	evidence PermissionEvidence,
) PermissionOutcome {
	if !evidence.Matched() || r == nil {
		return PermissionOutcome{}
	}

	switch evidence.Kind {
	case permissionEvidenceNone:
		return PermissionOutcome{}
	case permissionEvidenceKnownActiveBoundary:
		return PermissionOutcome{
			Matched:      true,
			Kind:         permissionOutcomeNone,
			BoundaryPath: evidence.BoundaryPath,
			TriggerPath:  evidence.TriggerPath,
		}
	case permissionEvidenceFileDenied:
		return PermissionOutcome{
			Matched: true,
			Kind:    permissionOutcomeRecordFileFailure,
			RetryWorkFailure: &RetryWorkFailure{
				Path:          r.Path,
				ActionType:    r.ActionType,
				ConditionType: evidence.IssueType,
			},
		}
	case permissionEvidenceBoundaryDenied:
		scopeKey, kind := permissionScopeOutcomeForEvidence(evidence)
		return PermissionOutcome{
			Matched:  true,
			Kind:     kind,
			ScopeKey: scopeKey,
			RetryWorkFailure: &RetryWorkFailure{
				Path:          r.Path,
				ActionType:    r.ActionType,
				ConditionType: evidence.IssueType,
				ScopeKey:      scopeKey,
				Blocked:       true,
			},
			BoundaryPath: evidence.BoundaryPath,
			TriggerPath:  evidence.TriggerPath,
		}
	default:
		panic("unknown permission evidence kind")
	}
}

func permissionScopeOutcomeForEvidence(evidence PermissionEvidence) (ScopeKey, PermissionOutcomeKind) {
	switch evidence.IssueType {
	case IssueLocalReadDenied:
		return SKPermLocalRead(evidence.BoundaryPath), permissionOutcomeActivateBoundaryScope
	case IssueLocalWriteDenied:
		return SKPermLocalWrite(evidence.BoundaryPath), permissionOutcomeActivateBoundaryScope
	case IssueRemoteWriteDenied:
		return SKPermRemoteWrite(evidence.BoundaryPath), permissionOutcomeActivateDerivedScope
	default:
		panic("unknown permission evidence issue type")
	}
}
