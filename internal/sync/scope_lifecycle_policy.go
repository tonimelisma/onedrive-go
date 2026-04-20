package sync

import "time"

const localScopeTrialInterval = 60 * time.Second

type timedBlockScopeAction int

const (
	timedBlockScopeKeep timedBlockScopeAction = iota
	timedBlockScopeDiscard
)

type persistedScopeFacts struct {
	blockedRetryCountByScope map[ScopeKey]int
}

type persistedScopeNormalizationStep struct {
	Key  ScopeKey
	Note string
}

type scopeTrialOutcome int

const (
	scopeTrialOutcomeRelease scopeTrialOutcome = iota
	scopeTrialOutcomeExtend
	scopeTrialOutcomeRearmOrDiscard
	scopeTrialOutcomeShutdown
	scopeTrialOutcomeFatal
)

func planPersistedScopeNormalization(
	blocks []*BlockScope,
	blockedRetries []RetryWorkRow,
) []persistedScopeNormalizationStep {
	facts := summarizePersistedBlockedRetries(blockedRetries)
	plan := make([]persistedScopeNormalizationStep, 0, len(blocks))

	for i := range blocks {
		block := blocks[i]
		if block == nil {
			continue
		}
		if decideTimedBlockScopeAction(facts.blockedRetryCountByScope[block.Key] > 0) == timedBlockScopeKeep {
			continue
		}

		plan = append(plan, persistedScopeNormalizationStep{
			Key:  block.Key,
			Note: "discarded scope without blocked retry work",
		})
	}

	return plan
}

func summarizePersistedBlockedRetries(rows []RetryWorkRow) persistedScopeFacts {
	facts := persistedScopeFacts{
		blockedRetryCountByScope: make(map[ScopeKey]int),
	}

	for i := range rows {
		if rows[i].ScopeKey.IsZero() || !rows[i].Blocked {
			continue
		}
		facts.blockedRetryCountByScope[rows[i].ScopeKey]++
	}

	return facts
}

func decideTimedBlockScopeAction(hasBlockedRetryWork bool) timedBlockScopeAction {
	if hasBlockedRetryWork {
		return timedBlockScopeKeep
	}

	return timedBlockScopeDiscard
}

func shouldKeepTimedBlockScope(hasBlockedRetryWork bool) bool {
	return decideTimedBlockScopeAction(hasBlockedRetryWork) == timedBlockScopeKeep
}

// computeTrialInterval is the single source of truth for timed blocker
// scheduling. Locally timed scopes use a fixed cadence; server Retry-After
// remains authoritative when present.
func computeTrialInterval(retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	return localScopeTrialInterval
}

func evaluateScopeTrialOutcome(
	trialScopeKey ScopeKey,
	decision *ResultDecision,
) scopeTrialOutcome {
	switch decision.TrialHint {
	case trialHintRelease:
		return scopeTrialOutcomeRelease
	case trialHintExtendOnMatchingScope:
		if trialScopeStillMatches(trialScopeKey, decision) {
			return scopeTrialOutcomeExtend
		}
		return scopeTrialOutcomeRearmOrDiscard
	case trialHintReclassify:
		return scopeTrialOutcomeRearmOrDiscard
	case trialHintShutdown:
		return scopeTrialOutcomeShutdown
	case trialHintFatal:
		return scopeTrialOutcomeFatal
	}

	panic("unknown trial hint")
}

func trialScopeStillMatches(
	trialScopeKey ScopeKey,
	decision *ResultDecision,
) bool {
	return !decision.ScopeEvidence.IsZero() && decision.ScopeEvidence == trialScopeKey
}
