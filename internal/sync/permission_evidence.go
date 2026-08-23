package sync

type permissionEvidenceKind int

const (
	permissionEvidenceNone permissionEvidenceKind = iota
	permissionEvidenceFileDenied
	permissionEvidenceBoundaryDenied
	permissionEvidenceKnownActiveBoundary
)

// permissionEvidence is the pure probe-layer result for one permission check.
// It carries only observed facts; the engine-owned runtime permission handlers
// decide persistence, blocking, and logging from this evidence.
type permissionEvidence struct {
	Kind         permissionEvidenceKind
	TriggerPath  string
	BoundaryPath string
	IssueType    string
	LastError    string
	HTTPStatus   int
}
