package sync

// LocalObservationRules is the policy local observation applies while walking
// the sync tree.

// LocalObservationRules controls platform-derived local validation semantics.
// These are not user-configured exclusions; they encode rules that depend on
// the target drive type or sync surface.
type LocalObservationRules struct {
	RejectSharePointRootForms bool
	// PersonalNameRules applies OneDrive Personal's name rules, which accept
	// names Business and SharePoint reserve: lockfiles such as yarn.lock,
	// desktop.ini, and Office "~$" owner files (verified against Graph).
	PersonalNameRules bool
}
