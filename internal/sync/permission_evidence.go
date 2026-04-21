package sync

type PermissionEvidenceKind int

const (
	permissionEvidenceNone PermissionEvidenceKind = iota
	permissionEvidenceFileDenied
	permissionEvidenceBoundaryDenied
	permissionEvidenceKnownActiveBoundary
)

// PermissionEvidence is the pure probe-layer result for one permission check.
// It carries only observed facts; persistence and logging policy live
// downstream in permission_policy.go and permission_decisions.go.
type PermissionEvidence struct {
	Kind         PermissionEvidenceKind
	TriggerPath  string
	BoundaryPath string
	IssueType    string
	LastError    string
	HTTPStatus   int
}

func (e PermissionEvidence) Matched() bool {
	return e.Kind != permissionEvidenceNone
}
