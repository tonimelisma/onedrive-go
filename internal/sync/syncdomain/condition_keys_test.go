package syncdomain

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
		ConditionKeyForStoredCondition(IssueInvalidFilename, ScopeKey{}))
	assert.Equal(t, ConditionRemoteWriteDenied,
		ConditionKeyForStoredCondition(IssueRemoteWriteDenied, ScopeKey{}))
	assert.Equal(t, ConditionServiceOutage,
		ConditionKeyForStoredCondition("", SKService()))
	assert.Equal(t, ConditionQuotaExceeded,
		ConditionKeyForStoredCondition("", SKQuotaOwn()))
	assert.Equal(t, ConditionRateLimited,
		ConditionKeyForStoredCondition("", SKThrottleDrive(driveid.New("0000000000000001"))))
	assert.Equal(t, ConditionUnexpectedCondition,
		ConditionKeyForStoredCondition("", ScopeKey{Kind: ScopeKeyKind(99)}))
	assert.Empty(t, ConditionKeyForStoredCondition("", ScopeKey{}))
}

// A condition no IssueType member names is a defect the stores refuse, so it
// is the unexpected condition even under a scope that has a condition of its
// own: taking the scope's would file the defect under a condition it is not.
// Validates: R-6.8.16
func TestConditionKeyForStoredCondition_ValueOutsideTheEnumIsUnexpectedUnderAnyScope(t *testing.T) {
	t.Parallel()

	require.False(t, IssueType(issueTypeOutsideTheEnum).IsMember())

	for _, scopeKey := range []ScopeKey{{}, SKQuotaOwn(), SKService(), {Kind: ScopeKeyKind(99)}} {
		assert.Equal(t, ConditionUnexpectedCondition, ConditionKeyForStoredCondition(issueTypeOutsideTheEnum, scopeKey),
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
				ConditionRemoteRootNotFound,
				ConditionLocalReadDenied, ConditionLocalWriteDenied, ConditionInvalidFilename, ConditionPathTooLong,
				ConditionFileTooLarge, ConditionCaseCollision, ConditionRemoteNameTwin, ConditionDiskFull,
				ConditionHashError, ConditionFileTooLargeForSpace, ConditionLocalNameTooLong, ConditionLocalReadOnly,
				ConditionDeletesHeld, ConditionLocalUnsyncable, ConditionLocalMountBoundary, ConditionLocalMountMissing,
				ConditionLocalInterrupted, ConditionRemovedFolderKept,
				ConditionRemoteFolderUnverified, ConditionLocalFileTooLarge, ConditionLocalBusy, ConditionLocalCrossDevice,
				ConditionUnsupportedOperation, ConditionMalwareDetected, ConditionLocalDatabaseOpen, ConditionUploadVerification,
				ConditionDownloadIntegrity, ConditionUnexpectedCondition,
			},
		},
	), ConditionClearsAutomatically)

	assert.False(t, ConditionClearsAutomatically(conditionKeyOutsideTheSet), "a key outside the set")
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
	assert.Equal(t, ConditionRemoteReadDenied, mustConditionKeyForIssueType(t, IssueRemoteReadDenied))
	assert.Equal(t, ConditionLocalReadDenied, mustConditionKeyForIssueType(t, IssueLocalReadDenied))
	assert.Equal(t, ConditionLocalWriteDenied, mustConditionKeyForIssueType(t, IssueLocalWriteDenied))
	assert.Equal(t, ConditionInvalidFilename, mustConditionKeyForIssueType(t, IssueInvalidFilename))
	assert.Equal(t, ConditionPathTooLong, mustConditionKeyForIssueType(t, IssuePathTooLong))
	assert.Equal(t, ConditionFileTooLarge, mustConditionKeyForIssueType(t, IssueFileTooLarge))
	assert.Equal(t, ConditionCaseCollision, mustConditionKeyForIssueType(t, IssueCaseCollision))
	assert.Equal(t, ConditionDiskFull, mustConditionKeyForIssueType(t, IssueDiskFull))
	assert.Equal(t, ConditionFileTooLargeForSpace, mustConditionKeyForIssueType(t, IssueFileTooLargeForSpace))
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
		IssuePathTooLong:                ConditionPathTooLong,
		IssueFileTooLarge:               ConditionFileTooLarge,
		IssueHashPanic:                  ConditionHashError,
		IssueUnauthorized:               ConditionAuthenticationRequired,
		IssueRemoteWriteDenied:          ConditionRemoteWriteDenied,
		IssueRemoteReadDenied:           ConditionRemoteReadDenied,
		IssueRemoteRootNotFound:         ConditionRemoteRootNotFound,
		IssueQuotaExceeded:              ConditionQuotaExceeded,
		IssueRateLimited:                ConditionRateLimited,
		IssueLocalReadDenied:            ConditionLocalReadDenied,
		IssueLocalWriteDenied:           ConditionLocalWriteDenied,
		IssueCaseCollision:              ConditionCaseCollision,
		IssueRemoteNameTwin:             ConditionRemoteNameTwin,
		IssueDiskFull:                   ConditionDiskFull,
		IssueServiceOutage:              ConditionServiceOutage,
		IssueFileTooLargeForSpace:       ConditionFileTooLargeForSpace,
		IssueLocalNameTooLong:           ConditionLocalNameTooLong,
		IssueLocalReadOnly:              ConditionLocalReadOnly,
		IssueDeletesHeld:                ConditionDeletesHeld,
		IssueLocalUnsyncableReplacement: ConditionLocalUnsyncable,
		IssueLocalMountBoundary:         ConditionLocalMountBoundary,
		IssueLocalMountMissing:          ConditionLocalMountMissing,
		IssueRemoteFolderUnverifiable:   ConditionRemoteFolderUnverified,
		IssueLocalInterruptedRemoval:    ConditionLocalInterrupted,
		IssueRemovedFolderKept:          ConditionRemovedFolderKept,
		IssueLocalFileTooLarge:          ConditionLocalFileTooLarge,
		IssueLocalBusy:                  ConditionLocalBusy,
		IssueLocalCrossDevice:           ConditionLocalCrossDevice,
		IssueUnsupportedOperation:       ConditionUnsupportedOperation,
		IssueMalwareDetected:            ConditionMalwareDetected,
		IssueLocalDatabaseOpen:          ConditionLocalDatabaseOpen,
		IssueUploadVerification:         ConditionUploadVerification,
		IssueDownloadIntegrity:          ConditionDownloadIntegrity,
		// Transient runtime conditions: no family of their own.
		IssueRequestTimeout:      "",
		IssueTransientConflict:   "",
		IssueTransientNotFound:   "",
		IssueResourceLocked:      "",
		IssueTransferURLRejected: "",
	}

	requireEveryIssueTypeDecided(t, families, func(member IssueType) ConditionKey {
		key, ok := ConditionKeyForIssueType(member)
		assert.Equal(t, key != "", ok, "%q: ok says whether a family is named", member)

		return key
	})

	key, ok := ConditionKeyForIssueType(issueTypeOutsideTheEnum)
	assert.True(t, ok)
	assert.Equal(t, ConditionUnexpectedCondition, key, "a value outside the enum")

	key, ok = ConditionKeyForIssueType("")
	assert.False(t, ok, "the empty value is no condition")
	assert.Empty(t, key)
}

func mustConditionKeyForIssueType(t *testing.T, issueType IssueType) ConditionKey {
	t.Helper()

	key, ok := ConditionKeyForIssueType(issueType)
	assert.True(t, ok)

	return key
}
