package cli

// defaultVisiblePaths caps sampled per-mount status rows and condition paths
// when --verbose is not set.
const defaultVisiblePaths = 5

type statusConditionJSON struct {
	ConditionKey  string   `json:"-"`
	ConditionType string   `json:"type"`
	Title         string   `json:"title"`
	Reason        string   `json:"reason"`
	Action        string   `json:"action"`
	ScopeKind     string   `json:"scope_kind,omitempty"`
	Scope         string   `json:"scope,omitempty"`
	Count         int      `json:"count"`
	Paths         []string `json:"paths,omitempty"`
}
