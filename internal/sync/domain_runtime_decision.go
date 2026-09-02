package sync

import "github.com/tonimelisma/onedrive-go/internal/errclass"

// This file holds the vocabulary the runtime uses to describe what it decided
// about a finished action. It is domain rather than engine because the store
// reads these values back off persisted rows: keeping them with the engine made
// the store appear to depend on runtime orchestration when it depends only on
// the words that orchestration writes down.

// resultPersistenceMode says whether a classified result owes a durable write.
type resultPersistenceMode int

const (
	persistNone resultPersistenceMode = iota
	persistRetryWork
)

// trialHint says what a timed scope trial should do with its scope next.
type trialHint int

const (
	trialHintRelease trialHint = iota
	trialHintExtendOnMatchingScope
	trialHintReclassify
	trialHintShutdown
	trialHintFatal
)

// resultDecision is the single classification output consumed by result
// routing. The decision is behavior-complete so downstream code does not
// re-derive policy from raw HTTP/local error facts.
type resultDecision struct {
	Class             errclass.Class
	ConditionKey      ConditionKey
	ScopeKey          ScopeKey
	ScopeEvidence     ScopeKey
	Persistence       resultPersistenceMode
	RunScopeDetection bool
	RecordSuccess     bool
	TrialHint         trialHint
	ConditionType     string
}
