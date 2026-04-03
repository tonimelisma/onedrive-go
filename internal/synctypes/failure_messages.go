// Package synctypes defines shared sync types, issue classes, and data contracts.
//
// Each issue type maps to a title, reason, and suggested action. This is the
// single source of truth for all user-facing failure text (R-2.3.8).
package synctypes

// IssueMessage holds the user-facing text for each issue type.
type IssueMessage struct {
	Title  string // e.g. "QUOTA EXCEEDED"
	Reason string // why it happened
	Action string // what user can do
}

// MessageForIssueType returns the human-readable message for a given issue type.
// Returns a generic message for unknown types.
func MessageForIssueType(issueType string) IssueMessage {
	key, ok := summaryKeyForIssueType(issueType)
	if !ok {
		key = SummarySyncFailure
	}
	descriptor := DescribeSummary(key)

	return IssueMessage{
		Title:  descriptor.Title,
		Reason: descriptor.Reason,
		Action: descriptor.Action,
	}
}
