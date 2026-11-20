package syncstore

import (
	"time"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

const localScopeTrialInterval = 60 * time.Second

type timedBlockScopeAction int

const (
	TimedBlockScopeKeep timedBlockScopeAction = iota
	TimedBlockScopeDiscard
)

type persistedScopeFacts struct {
	blockedRetryCountByScope map[syncdomain.ScopeKey]int
}

type persistedScopeNormalizationStep struct {
	Key  syncdomain.ScopeKey
	Note string
}

type scopeTrialOutcome int

const (
	ScopeTrialOutcomeRelease scopeTrialOutcome = iota
	ScopeTrialOutcomeExtend
	ScopeTrialOutcomeRearmOrDiscard
	ScopeTrialOutcomeShutdown
	ScopeTrialOutcomeFatal
)

func PlanPersistedScopeNormalization(
	blocks []*syncdomain.BlockScope,
	blockedRetries []syncdomain.RetryWorkRow,
) []persistedScopeNormalizationStep {
	facts := summarizePersistedBlockedRetries(blockedRetries)
	plan := make([]persistedScopeNormalizationStep, 0, len(blocks))

	for i := range blocks {
		block := blocks[i]
		if block == nil {
			continue
		}
		if DecideTimedBlockScopeAction(facts.blockedRetryCountByScope[block.Key] > 0) == TimedBlockScopeKeep {
			continue
		}

		plan = append(plan, persistedScopeNormalizationStep{
			Key:  block.Key,
			Note: "discarded scope without blocked retry work",
		})
	}

	return plan
}

func summarizePersistedBlockedRetries(rows []syncdomain.RetryWorkRow) persistedScopeFacts {
	facts := persistedScopeFacts{
		blockedRetryCountByScope: make(map[syncdomain.ScopeKey]int),
	}

	for i := range rows {
		if rows[i].ScopeKey.IsZero() || !rows[i].Blocked {
			continue
		}
		facts.blockedRetryCountByScope[rows[i].ScopeKey]++
	}

	return facts
}

func DecideTimedBlockScopeAction(hasBlockedRetryWork bool) timedBlockScopeAction {
	if hasBlockedRetryWork {
		return TimedBlockScopeKeep
	}

	return TimedBlockScopeDiscard
}

func shouldKeepTimedBlockScope(hasBlockedRetryWork bool) bool {
	return DecideTimedBlockScopeAction(hasBlockedRetryWork) == TimedBlockScopeKeep
}

// ComputeTrialInterval is the single source of truth for timed blocker
// scheduling. Locally timed scopes use a fixed cadence; server Retry-After
// remains authoritative when present.
func ComputeTrialInterval(retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	return localScopeTrialInterval
}

func EvaluateScopeTrialOutcome(
	trialScopeKey syncdomain.ScopeKey,
	decision *syncdomain.ResultDecision,
) scopeTrialOutcome {
	switch decision.TrialHint {
	case syncdomain.TrialHintRelease:
		return ScopeTrialOutcomeRelease
	case syncdomain.TrialHintExtendOnMatchingScope:
		if trialScopeStillMatches(trialScopeKey, decision) {
			return ScopeTrialOutcomeExtend
		}
		return ScopeTrialOutcomeRearmOrDiscard
	case syncdomain.TrialHintReclassify:
		return ScopeTrialOutcomeRearmOrDiscard
	case syncdomain.TrialHintShutdown:
		return ScopeTrialOutcomeShutdown
	case syncdomain.TrialHintFatal:
		return ScopeTrialOutcomeFatal
	}

	panic("unknown trial hint")
}

func trialScopeStillMatches(
	trialScopeKey syncdomain.ScopeKey,
	decision *syncdomain.ResultDecision,
) bool {
	return !decision.ScopeEvidence.IsZero() && decision.ScopeEvidence == trialScopeKey
}
