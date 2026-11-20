package syncstore

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.10.33
func TestPlanPersistedScopeNormalization_DiscardsOnlyScopesWithoutBlockedRetryWork(t *testing.T) {
	t.Parallel()

	scopeWithWork := syncdomain.SKPermRemoteWrite("Shared/Docs")
	scopeWithoutWork := syncdomain.SKDiskLocal()

	plan := PlanPersistedScopeNormalization(
		[]*syncdomain.BlockScope{
			{Key: scopeWithWork},
			{Key: scopeWithoutWork},
			nil,
		},
		[]syncdomain.RetryWorkRow{
			{
				Path:       "Shared/Docs/file.txt",
				ActionType: syncdomain.ActionUpload,
				ScopeKey:   scopeWithWork,
				Blocked:    true,
			},
			{
				Path:       "ready.txt",
				ActionType: syncdomain.ActionUpload,
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

	trialScopeKey := syncdomain.SKService()

	assert.Equal(t, ScopeTrialOutcomeRelease, EvaluateScopeTrialOutcome(trialScopeKey, &syncdomain.ResultDecision{
		TrialHint: syncdomain.TrialHintRelease,
	}))
	assert.Equal(t, ScopeTrialOutcomeExtend, EvaluateScopeTrialOutcome(trialScopeKey, &syncdomain.ResultDecision{
		TrialHint:     syncdomain.TrialHintExtendOnMatchingScope,
		ScopeEvidence: trialScopeKey,
	}))
	assert.Equal(t, ScopeTrialOutcomeRearmOrDiscard, EvaluateScopeTrialOutcome(trialScopeKey, &syncdomain.ResultDecision{
		TrialHint:     syncdomain.TrialHintExtendOnMatchingScope,
		ScopeEvidence: syncdomain.SKQuotaOwn(),
	}))
}
