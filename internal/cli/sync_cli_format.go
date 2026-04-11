package cli

import "time"

// defaultVisiblePaths caps per-group path display in the detailed single-drive
// status view when --verbose is not set.
const defaultVisiblePaths = 5

type failureGroupJSON struct {
	IssueType string   `json:"issue_type"`
	Title     string   `json:"title"`
	Reason    string   `json:"reason"`
	Action    string   `json:"action"`
	Scope     string   `json:"scope,omitempty"`
	Count     int      `json:"count"`
	Paths     []string `json:"paths"`
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
