package sync

type PermissionEvidenceKind int

const (
	permissionEvidenceNone PermissionEvidenceKind = iota
	permissionEvidenceFileDenied
	permissionEvidenceBoundaryDenied
	permissionEvidenceKnownActiveBoundary
)

// PermissionEvidence is the pure probe-layer result for one permission check.
// It carries only observed facts; the engine-owned runtime permission handlers
// decide persistence, blocking, and logging from this evidence.
type PermissionEvidence struct {
	Kind         PermissionEvidenceKind
	TriggerPath  string
	BoundaryPath string
	IssueType    string
	LastError    string
	HTTPStatus   int
}
