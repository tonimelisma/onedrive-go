package cli

import "time"

// defaultVisiblePaths caps sampled per-drive status rows and issue-group paths
// when --verbose is not set.
const defaultVisiblePaths = 5

type failureGroupJSON struct {
	SummaryKey string   `json:"summary_key,omitempty"`
	IssueType  string   `json:"issue_type"`
	Title      string   `json:"title"`
	Reason     string   `json:"reason"`
	Action     string   `json:"action"`
	ScopeKind  string   `json:"scope_kind,omitempty"`
	Scope      string   `json:"scope,omitempty"`
	Count      int      `json:"count"`
	Paths      []string `json:"paths"`
}

func itemNoun(n int) string {
	if n == 1 {
		return "item"
	}

	return "items"
}

func formatNanoTimestamp(nanos int64) string {
	if nanos == 0 {
		return ""
	}

	return time.Unix(0, nanos).UTC().Format(time.RFC3339)
}
