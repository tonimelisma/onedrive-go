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

	plan := PlanPersistedScopeNormalization(
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

	assert.Equal(t, ScopeTrialOutcomeRelease, EvaluateScopeTrialOutcome(trialScopeKey, &ResultDecision{
		TrialHint: TrialHintRelease,
	}))
	assert.Equal(t, ScopeTrialOutcomeExtend, EvaluateScopeTrialOutcome(trialScopeKey, &ResultDecision{
		TrialHint:     TrialHintExtendOnMatchingScope,
		ScopeEvidence: trialScopeKey,
	}))
	assert.Equal(t, ScopeTrialOutcomeRearmOrDiscard, EvaluateScopeTrialOutcome(trialScopeKey, &ResultDecision{
		TrialHint:     TrialHintExtendOnMatchingScope,
		ScopeEvidence: SKQuotaOwn(),
	}))
}
