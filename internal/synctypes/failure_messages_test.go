package synctypes

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
)

// Validates: R-2.3.7
func TestMessageForIssueType_AllKnownTypes(t *testing.T) {
	t.Parallel()

	knownTypes := []string{
		IssueQuotaExceeded,
		IssueUnauthorized,
		IssueRateLimited,
		IssuePermissionDenied,
		IssueSharedFolderBlocked,
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

func TestMessageForIssueType_UnauthorizedDelegatesToAuthState(t *testing.T) {
	t.Parallel()

	msg := MessageForIssueType(IssueUnauthorized)
	presentation := authstate.UnauthorizedIssuePresentation()

	assert.Equal(t, "AUTHENTICATION REQUIRED", msg.Title)
	assert.Equal(t, presentation.Reason, msg.Reason)
	assert.Equal(t, presentation.Action, msg.Action)
}

// Validates: R-6.6.11
func TestMessageForFailure_QuotaShortcutUsesOwnerSpecificCopy(t *testing.T) {
	t.Parallel()

	msg := MessageForFailure(IssueQuotaExceeded, SKQuotaShortcut("drive:item"), "Team Docs")

	assert.Equal(t, "QUOTA EXCEEDED", msg.Title)
	assert.Equal(t, `Shared folder "Team Docs" owner's storage is full.`, msg.Reason)
	assert.Equal(t, "Ask the shared folder owner to free up space or upgrade their plan.", msg.Action)
}

// Validates: R-6.6.11
func TestMessageForFailure_QuotaOwnKeepsOwnDriveCopy(t *testing.T) {
	t.Parallel()

	msg := MessageForFailure(IssueQuotaExceeded, SKQuotaOwn(), "your OneDrive storage")
	base := MessageForIssueType(IssueQuotaExceeded)

	assert.Equal(t, base.Title, msg.Title)
	assert.Equal(t, base.Reason, msg.Reason)
	assert.Equal(t, base.Action, msg.Action)
}
