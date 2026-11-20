package syncdomain

import "github.com/tonimelisma/onedrive-go/internal/errclass"

// This file holds the vocabulary the runtime uses to describe what it decided
// about a finished action. It is domain rather than engine because the store
// reads these values back off persisted rows: keeping them with the engine made
// the store appear to depend on runtime orchestration when it depends only on
// the words that orchestration writes down.

// resultPersistenceMode says whether a classified result owes a durable write.
type resultPersistenceMode int

const (
	PersistNone resultPersistenceMode = iota
	PersistRetryWork
)

// trialHint says what a timed scope trial should do with its scope next.
type trialHint int

const (
	TrialHintRelease trialHint = iota
	TrialHintExtendOnMatchingScope
	TrialHintReclassify
	TrialHintShutdown
	TrialHintFatal
)

// ResultDecision is the single classification output consumed by result
// routing. The decision is behavior-complete so downstream code does not
// re-derive policy from raw HTTP/local error facts.
type ResultDecision struct {
	Class             errclass.Class
	ConditionKey      ConditionKey
	ScopeKey          ScopeKey
	ScopeEvidence     ScopeKey
	Persistence       resultPersistenceMode
	RunScopeDetection bool
	RecordSuccess     bool
	TrialHint         trialHint
	ConditionType     IssueType
}

// RetryWorkScope is the scope a failure's retry row records. Blocked work is
// recorded under the scope its decision blocks (ScopeKey), the scope that is
// activated, so the two can never name different scopes; other work records
// its evidence, which a later activation of that scope may block.
func (decision *ResultDecision) RetryWorkScope() ScopeKey {
	if decision.Class == errclass.ClassScopeBlockingTransient && !decision.ScopeKey.IsZero() {
		return decision.ScopeKey
	}

	return decision.ScopeEvidence
}
