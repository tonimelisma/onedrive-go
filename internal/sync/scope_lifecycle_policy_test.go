package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Validates: R-2.10.33
func TestPlanPersistedScopeNormalization_DiscardsOnlyScopesWithoutBlockedRetryWork(t *testing.T) {
	t.Parallel()

	scopeWithWork := SKPermRemoteWrite("Shared/Docs")
	scopeWithoutWork := SKDiskLocal()

	plan := planPersistedScopeNormalization(
		[]*BlockScope{
			{Key: scopeWithWork},
			{Key: scopeWithoutWork},
			nil,
		},
		[]RetryWorkRow{
			{
				Path:       "Shared/Docs/file.txt",
				ActionType: ActionUpload,
				ScopeKey:   scopeWithWork,
				Blocked:    true,
			},
			{
				Path:       "ready.txt",
				ActionType: ActionUpload,
				ScopeKey:   scopeWithoutWork,
				Blocked:    false,
			},
		},
	)

	assert.Equal(t, []persistedScopeNormalizationStep{
		{
			Key:  scopeWithoutWork,
			Note: "discarded scope without blocked retry work",
		},
	}, plan)
}

// Validates: R-2.10.5
func TestEvaluateScopeTrialOutcome_UsesSharedLifecyclePolicy(t *testing.T) {
	t.Parallel()

	trialScopeKey := SKService()

	assert.Equal(t, scopeTrialOutcomeRelease, evaluateScopeTrialOutcome(trialScopeKey, &ResultDecision{
		TrialHint: trialHintRelease,
	}))
	assert.Equal(t, scopeTrialOutcomeExtend, evaluateScopeTrialOutcome(trialScopeKey, &ResultDecision{
		TrialHint:     trialHintExtendOnMatchingScope,
		ScopeEvidence: trialScopeKey,
	}))
	assert.Equal(t, scopeTrialOutcomeRearmOrDiscard, evaluateScopeTrialOutcome(trialScopeKey, &ResultDecision{
		TrialHint:     trialHintExtendOnMatchingScope,
		ScopeEvidence: SKQuotaOwn(),
	}))
}
