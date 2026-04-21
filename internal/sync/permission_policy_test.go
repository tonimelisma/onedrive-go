package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.14.1
func TestDecidePermissionOutcome_FileDenied(t *testing.T) {
	t.Parallel()

	outcome := DecidePermissionOutcome(&ActionCompletion{
		Path:       "docs/file.txt",
		ActionType: ActionUpload,
	}, PermissionEvidence{
		Kind:        permissionEvidenceFileDenied,
		TriggerPath: "docs/file.txt",
		IssueType:   IssueLocalWriteDenied,
		LastError:   "file not accessible",
	})

	require.True(t, outcome.Matched)
	assert.Equal(t, permissionOutcomeRecordFileFailure, outcome.Kind)
	require.NotNil(t, outcome.RetryWorkFailure)
	assert.Equal(t, "docs/file.txt", outcome.RetryWorkFailure.Path)
	assert.Equal(t, IssueLocalWriteDenied, outcome.RetryWorkFailure.ConditionType)
	assert.True(t, outcome.ScopeKey.IsZero())
}

// Validates: R-2.14.1
func TestDecidePermissionOutcome_BoundaryDenied(t *testing.T) {
	t.Parallel()

	outcome := DecidePermissionOutcome(&ActionCompletion{
		Path:       "blocked/file.txt",
		ActionType: ActionUpload,
	}, PermissionEvidence{
		Kind:         permissionEvidenceBoundaryDenied,
		BoundaryPath: "blocked",
		TriggerPath:  "blocked/file.txt",
		IssueType:    IssueRemoteWriteDenied,
		LastError:    "folder is read-only",
		HTTPStatus:   403,
	})

	require.True(t, outcome.Matched)
	assert.Equal(t, permissionOutcomeActivateDerivedScope, outcome.Kind)
	assert.Equal(t, SKPermRemoteWrite("blocked"), outcome.ScopeKey)
	require.NotNil(t, outcome.RetryWorkFailure)
	assert.Equal(t, SKPermRemoteWrite("blocked"), outcome.RetryWorkFailure.ScopeKey)
	assert.True(t, outcome.RetryWorkFailure.Blocked)
	assert.Equal(t, "blocked", outcome.BoundaryPath)
	assert.Equal(t, "blocked/file.txt", outcome.TriggerPath)
}

// Validates: R-2.14.1
func TestDecidePermissionOutcome_KnownActiveBoundary(t *testing.T) {
	t.Parallel()

	outcome := DecidePermissionOutcome(&ActionCompletion{
		Path:       "blocked/file.txt",
		ActionType: ActionUpload,
	}, PermissionEvidence{
		Kind:         permissionEvidenceKnownActiveBoundary,
		BoundaryPath: "blocked",
		TriggerPath:  "blocked/file.txt",
		IssueType:    IssueRemoteWriteDenied,
	})

	require.True(t, outcome.Matched)
	assert.Equal(t, permissionOutcomeNone, outcome.Kind)
	assert.Nil(t, outcome.RetryWorkFailure)
	assert.True(t, outcome.ScopeKey.IsZero())
	assert.Equal(t, "blocked", outcome.BoundaryPath)
}
