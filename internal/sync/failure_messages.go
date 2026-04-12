// Package sync defines sync-domain failure messaging and issue descriptors.
package sync

import (
	"fmt"
)

// IssueMessage holds the user-facing text for each issue type.
type IssueMessage struct {
	Title  string // e.g. "QUOTA EXCEEDED"
	Reason string // why it happened
	Action string // what user can do
}

// MessageForFailure returns the user-facing message for a failure, including
// any scope-specific wording that changes who owns the remediation.
func MessageForFailure(issueType string, scopeKey ScopeKey, scopeLabel string) IssueMessage {
	message := MessageForIssueType(issueType)

	if issueType == IssueQuotaExceeded && scopeKey.Kind == ScopeQuotaShortcut {
		if scopeLabel == "" {
			message.Reason = "The shared folder owner's storage is full."
		} else {
			message.Reason = fmt.Sprintf("Shared folder %q owner's storage is full.", scopeLabel)
		}

		message.Action = "Ask the shared folder owner to free up space or upgrade their plan."
	}

	return message
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
