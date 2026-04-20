package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.10.33
func TestEngineFlow_AssertPersistedInvariants_RejectsOrphanBlockScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	require.NoError(t, eng.baseline.UpsertBlockScope(t.Context(), &BlockScope{
		Key:           SKService(),
		BlockedAt:     eng.nowFn().Add(-time.Minute),
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFn().Add(time.Minute),
	}))

	err := flow.assertPersistedInvariants(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has no related observation_issues or retry_work rows")
}

// Validates: R-2.10.33
func TestEngineFlow_AssertPersistedInvariants_AllowsObservationBackedBlockScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)
	scopeKey := SKPermRemoteWrite("Shared/Docs")

	require.NoError(t, eng.baseline.UpsertBlockScope(t.Context(), &BlockScope{
		Key:           scopeKey,
		BlockedAt:     eng.nowFn().Add(-time.Minute),
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFn().Add(time.Minute),
	}))
	seedObservationIssueRowForTest(t, eng.baseline, &ObservationIssue{
		Path:       "Shared/Docs/file.txt",
		DriveID:    eng.driveID,
		ActionType: ActionUpload,
		IssueType:  IssueRemoteWriteDenied,
		Error:      "write denied",
		ScopeKey:   scopeKey,
	})

	require.NoError(t, flow.assertPersistedInvariants(t.Context()))
}
