package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.14.3, R-6.8.16
func TestConditionKeyForStoredCondition_RepresentativeMappings(t *testing.T) {
	t.Parallel()

	assert.Equal(t, ConditionInvalidFilename,
		conditionKeyForStoredCondition(IssueInvalidFilename, ScopeKey{}))
	assert.Equal(t, ConditionRemoteWriteDenied,
		conditionKeyForStoredCondition(IssueRemoteWriteDenied, ScopeKey{}))
	assert.Equal(t, ConditionServiceOutage,
		conditionKeyForStoredCondition("", SKService()))
	assert.Equal(t, ConditionQuotaExceeded,
		conditionKeyForStoredCondition("", SKQuotaOwn()))
	assert.Equal(t, ConditionRateLimited,
		conditionKeyForStoredCondition("", SKThrottleDrive(driveid.New("0000000000000001"))))
	assert.Equal(t, ConditionUnexpectedCondition,
		conditionKeyForStoredCondition("", ScopeKey{Kind: ScopeKeyKind(99)}))
	assert.Empty(t, conditionKeyForStoredCondition("", ScopeKey{}))
}

// A condition no IssueType member names is a defect the stores refuse, so it
// is the unexpected condition even under a scope that has a condition of its
// own: taking the scope's would file the defect under a condition it is not.
// Validates: R-6.8.16
func TestConditionKeyForStoredCondition_ValueOutsideTheEnumIsUnexpectedUnderAnyScope(t *testing.T) {
	t.Parallel()

	require.False(t, IssueType(issueTypeOutsideTheEnum).isMember())

	for _, scopeKey := range []ScopeKey{{}, SKQuotaOwn(), SKService(), {Kind: ScopeKeyKind(99)}} {
		assert.Equal(t, ConditionUnexpectedCondition, conditionKeyForStoredCondition(issueTypeOutsideTheEnum, scopeKey),
			"under scope %q", scopeKey.String())
	}
}

// Every declared condition key has a display rank of its own, ahead of the
// rank a key no constant declares gets, so a new key cannot sort among
// unknown keys because the ordered list was not given it.
// Validates: R-6.8.16
func TestConditionKeyRank_EveryDeclaredKeyHasItsOwnRank(t *testing.T) {
	t.Parallel()

	undeclaredRank := conditionKeyRank(conditionKeyOutsideTheSet)
	nameByRank := make(map[int]string)
	for name, value := range stringConstantsOfType(t, "condition_keys.go", "ConditionKey") {
		rank := conditionKeyRank(ConditionKey(value))
		assert.Less(t, rank, undeclaredRank, "%s has no rank in the display order", name)
		other, shared := nameByRank[rank]
		assert.False(t, shared, "%s and %s share a rank", name, other)
		nameByRank[rank] = name
	}
}

// Whether each condition clears without the user, the fact that keeps the
// watch summary below a warning and sets the block-scope line's level. The
// table must name every declared key, so a new key is decided on purpose; a
// key outside the set is not known to clear by itself, so it is something
// the user may need to act on.
// Validates: R-6.6.30
func TestConditionClearsAutomatically_EveryKeyIsDecided(t *testing.T) {
	t.Parallel()

	requireEveryConstantDecided(t, "condition_keys.go", "ConditionKey", verdictTable(t,
		verdictGroup[ConditionKey, bool]{
			verdict: true,
			members: []ConditionKey{ConditionRateLimited, ConditionServiceOutage},
		},
		verdictGroup[ConditionKey, bool]{
			verdict: false,
			members: []ConditionKey{
				ConditionAuthenticationRequired, ConditionQuotaExceeded, ConditionRemoteWriteDenied, ConditionRemoteReadDenied,
				ConditionLocalReadDenied, ConditionLocalWriteDenied, ConditionInvalidFilename, ConditionPathTooLong,
				ConditionFileTooLarge, ConditionCaseCollision, ConditionRemoteNameTwin, ConditionDiskFull,
				ConditionHashError, ConditionFileTooLargeForSpace, ConditionLocalNameTooLong, ConditionLocalReadOnly,
				ConditionDeletesHeld, ConditionLocalUnsyncable, ConditionLocalMountBoundary, ConditionLocalInterrupted,
				ConditionRemoteFolderUnverified, ConditionLocalFileTooLarge, ConditionLocalBusy, ConditionLocalCrossDevice,
				ConditionUnsupportedOperation, ConditionMalwareDetected, ConditionLocalDatabaseOpen, ConditionUploadVerification,
				ConditionDownloadIntegrity, ConditionUnexpectedCondition,
			},
		},
	), conditionClearsAutomatically)

	assert.False(t, conditionClearsAutomatically(conditionKeyOutsideTheSet), "a key outside the set")
}

// Validates: R-2.10.4, R-6.8.16
func TestConditionKeyLess_UsesCanonicalDisplayOrder(t *testing.T) {
	t.Parallel()

	assert.True(t, ConditionKeyLess(ConditionAuthenticationRequired, ConditionRemoteWriteDenied))
	assert.True(t, ConditionKeyLess(ConditionRemoteWriteDenied, ConditionInvalidFilename))
	assert.True(t, ConditionKeyLess(ConditionUnexpectedCondition, ConditionKey("zzz_custom")))
	assert.True(t, ConditionKeyLess(ConditionKey("aaa_custom"), ConditionKey("zzz_custom")))
	assert.False(t, ConditionKeyLess(ConditionKey("zzz_custom"), ConditionUnexpectedCondition))
}

// Validates: R-6.8.16
func TestConditionKeyForIssueType_RepresentativeMappings(t *testing.T) {
	t.Parallel()

	assert.Equal(t, ConditionAuthenticationRequired, mustConditionKeyForIssueType(t, IssueUnauthorized))
	assert.Equal(t, ConditionQuotaExceeded, mustConditionKeyForIssueType(t, IssueQuotaExceeded))
	assert.Equal(t, ConditionRemoteReadDenied, mustConditionKeyForIssueType(t, issueRemoteReadDenied))
	assert.Equal(t, ConditionLocalReadDenied, mustConditionKeyForIssueType(t, issueLocalReadDenied))
	assert.Equal(t, ConditionLocalWriteDenied, mustConditionKeyForIssueType(t, issueLocalWriteDenied))
	assert.Equal(t, ConditionInvalidFilename, mustConditionKeyForIssueType(t, IssueInvalidFilename))
	assert.Equal(t, ConditionPathTooLong, mustConditionKeyForIssueType(t, issuePathTooLong))
	assert.Equal(t, ConditionFileTooLarge, mustConditionKeyForIssueType(t, issueFileTooLarge))
	assert.Equal(t, ConditionCaseCollision, mustConditionKeyForIssueType(t, issueCaseCollision))
	assert.Equal(t, ConditionDiskFull, mustConditionKeyForIssueType(t, issueDiskFull))
	assert.Equal(t, ConditionFileTooLargeForSpace, mustConditionKeyForIssueType(t, issueFileTooLargeForSpace))
}

// The condition family of every issue type. The table must name every
// declared member, so a new member gets a family (or none) chosen on purpose
// here as well as in the exhaustive switch. A member with no family is a
// transient runtime condition no store keeps: its result is grouped by its
// class. A value outside the enum is the unexpected condition, never "no
// family", which used to let it fall through to whatever a caller tried next.
// Validates: R-6.8.16, R-2.10.4
func TestConditionKeyForIssueType_EveryMemberIsDecided(t *testing.T) {
	t.Parallel()

	families := map[IssueType]ConditionKey{
		IssueInvalidFilename:            ConditionInvalidFilename,
		issuePathTooLong:                ConditionPathTooLong,
		issueFileTooLarge:               ConditionFileTooLarge,
		issueHashPanic:                  ConditionHashError,
		IssueUnauthorized:               ConditionAuthenticationRequired,
		IssueRemoteWriteDenied:          ConditionRemoteWriteDenied,
		issueRemoteReadDenied:           ConditionRemoteReadDenied,
		IssueQuotaExceeded:              ConditionQuotaExceeded,
		issueRateLimited:                ConditionRateLimited,
		issueLocalReadDenied:            ConditionLocalReadDenied,
		issueLocalWriteDenied:           ConditionLocalWriteDenied,
		issueCaseCollision:              ConditionCaseCollision,
		issueRemoteNameTwin:             ConditionRemoteNameTwin,
		issueDiskFull:                   ConditionDiskFull,
		issueServiceOutage:              ConditionServiceOutage,
		issueFileTooLargeForSpace:       ConditionFileTooLargeForSpace,
		issueLocalNameTooLong:           ConditionLocalNameTooLong,
		issueLocalReadOnly:              ConditionLocalReadOnly,
		issueDeletesHeld:                ConditionDeletesHeld,
		issueLocalUnsyncableReplacement: ConditionLocalUnsyncable,
		issueLocalMountBoundary:         ConditionLocalMountBoundary,
		issueRemoteFolderUnverifiable:   ConditionRemoteFolderUnverified,
		issueLocalInterruptedRemoval:    ConditionLocalInterrupted,
		issueLocalFileTooLarge:          ConditionLocalFileTooLarge,
		issueLocalBusy:                  ConditionLocalBusy,
		issueLocalCrossDevice:           ConditionLocalCrossDevice,
		issueUnsupportedOperation:       ConditionUnsupportedOperation,
		issueMalwareDetected:            ConditionMalwareDetected,
		issueLocalDatabaseOpen:          ConditionLocalDatabaseOpen,
		issueUploadVerification:         ConditionUploadVerification,
		issueDownloadIntegrity:          ConditionDownloadIntegrity,
		// Transient runtime conditions: no family of their own.
		issueRequestTimeout:      "",
		issueTransientConflict:   "",
		issueTransientNotFound:   "",
		issueResourceLocked:      "",
		issueTransferURLRejected: "",
	}

	requireEveryIssueTypeDecided(t, families, func(member IssueType) ConditionKey {
		key, ok := conditionKeyForIssueType(member)
		assert.Equal(t, key != "", ok, "%q: ok says whether a family is named", member)

		return key
	})

	key, ok := conditionKeyForIssueType(issueTypeOutsideTheEnum)
	assert.True(t, ok)
	assert.Equal(t, ConditionUnexpectedCondition, key, "a value outside the enum")

	key, ok = conditionKeyForIssueType("")
	assert.False(t, ok, "the empty value is no condition")
	assert.Empty(t, key)
}

func mustConditionKeyForIssueType(t *testing.T, issueType IssueType) ConditionKey {
	t.Helper()

	key, ok := conditionKeyForIssueType(issueType)
	assert.True(t, ok)

	return key
}
