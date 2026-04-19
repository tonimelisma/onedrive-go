package cli

// defaultVisiblePaths caps sampled per-drive status rows and condition paths
// when --verbose is not set.
const defaultVisiblePaths = 5

type statusConditionJSON struct {
	SummaryKey    string   `json:"summary_key,omitempty"`
	ConditionType string   `json:"condition_type"`
	Title         string   `json:"title"`
	Reason        string   `json:"reason"`
	Action        string   `json:"action"`
	ScopeKind     string   `json:"scope_kind,omitempty"`
	Scope         string   `json:"scope,omitempty"`
	Count         int      `json:"count"`
	Paths         []string `json:"paths"`
}

func itemNoun(n int) string {
	if n == 1 {
		return "item"
	}

	return "items"
}
