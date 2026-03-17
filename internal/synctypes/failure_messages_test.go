package synctypes

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Validates: R-2.3.7
func TestMessageForIssueType_AllKnownTypes(t *testing.T) {
	t.Parallel()

	knownTypes := []string{
		IssueQuotaExceeded,
		IssuePermissionDenied,
		IssueLocalPermissionDenied,
		IssueInvalidFilename,
		IssuePathTooLong,
		IssueFileTooLarge,
		IssueBigDeleteHeld,
		IssueCaseCollision,
		IssueDiskFull,
		IssueServiceOutage,
		IssueHashPanic,
		IssueFileTooLargeForSpace,
	}

	for _, issueType := range knownTypes {
		msg := MessageForIssueType(issueType)
		assert.NotEmpty(t, msg.Title, "issue type %q should have a title", issueType)
		assert.NotEmpty(t, msg.Reason, "issue type %q should have a reason", issueType)
		assert.NotEmpty(t, msg.Action, "issue type %q should have an action", issueType)
	}
}

// Validates: R-2.3.7
func TestMessageForIssueType_UnknownType(t *testing.T) {
	t.Parallel()

	msg := MessageForIssueType("totally_unknown_type")
	assert.Equal(t, "SYNC FAILURE", msg.Title)
	assert.NotEmpty(t, msg.Reason)
	assert.NotEmpty(t, msg.Action)
}
