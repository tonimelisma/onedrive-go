package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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

func TestMessageForIssueType_UnknownType(t *testing.T) {
	t.Parallel()

	msg := MessageForIssueType("totally_unknown_type")
	assert.Equal(t, "SYNC FAILURE", msg.Title)
	assert.NotEmpty(t, msg.Reason)
	assert.NotEmpty(t, msg.Action)
}

func TestHumanizeScopeKey_ThrottleAccount(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "your OneDrive account (rate limited)", HumanizeScopeKey(SKThrottleAccount.String(), nil))
}

func TestHumanizeScopeKey_Service(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "OneDrive service", HumanizeScopeKey(SKService.String(), nil))
}

func TestHumanizeScopeKey_QuotaOwn(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "your OneDrive storage", HumanizeScopeKey(SKQuotaOwn.String(), nil))
}

// Validates: R-2.10.22
func TestHumanizeScopeKey_QuotaShortcut_FoundByLocalPath(t *testing.T) {
	t.Parallel()

	shortcuts := []Shortcut{
		{
			RemoteDrive: "driveAAA",
			RemoteItem:  "itemBBB",
			LocalPath:   "Team Docs",
			Observation: ObservationDelta,
		},
	}

	result := HumanizeScopeKey(SKQuotaShortcut("driveAAA:itemBBB").String(), shortcuts)
	assert.Equal(t, "Team Docs", result)
}

func TestHumanizeScopeKey_QuotaShortcut_NotFound(t *testing.T) {
	t.Parallel()

	// No shortcuts provided — falls back to the raw key suffix.
	result := HumanizeScopeKey(SKQuotaShortcut("driveXXX:itemYYY").String(), nil)
	assert.Equal(t, "driveXXX:itemYYY", result)
}

func TestHumanizeScopeKey_PermDir(t *testing.T) {
	t.Parallel()

	result := HumanizeScopeKey(SKPermDir("/home/user/OneDrive/Private").String(), nil)
	assert.Equal(t, "/home/user/OneDrive/Private", result)
}

func TestHumanizeScopeKey_Empty(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "", HumanizeScopeKey("", nil))
}

func TestHumanizeScopeKey_UnknownKey(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "some:unknown:key", HumanizeScopeKey("some:unknown:key", nil))
}
