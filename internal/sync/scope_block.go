package sync

import (
	"fmt"
	"time"
)

// BlockScope represents one active timed shared blocker as persisted in
// block_scopes for restart, admission, and read-side projections.
type BlockScope struct {
	Key           ScopeKey
	TrialInterval time.Duration // current interval between trial actions
	NextTrialAt   time.Time     // when to dispatch the next trial
	// SlowStartThreshold is the release ramp's memory of the last 429
	// (activeScope.SlowStartThreshold), persisted because one-shot passes
	// and restarts rebuild the runtime from this row (R-6.8.24).
	SlowStartThreshold int
}

// activeScope is the watch-runtime working-set shape. It keeps only the
// mutable timer/admission facts needed for in-memory scope ownership; the
// persisted scope row remains the durable/read-side shape.
type activeScope struct {
	Key           ScopeKey
	TrialInterval time.Duration
	NextTrialAt   time.Time

	// ReleaseAllowance is the ramp position of a scope on probation: zero
	// while the scope blocks, the size of the batch last released after a
	// successful trial once it does not (R-6.8.24). Runtime-only; a restart
	// starts a fresh trial.
	ReleaseAllowance int
	// BatchOutstanding is how many members of the last released batch have
	// not finished yet. The batch's last member to finish releases the next,
	// doubled batch; a due trial resets it, since a trial only fires when
	// the ramp stalled.
	BatchOutstanding int
	// SlowStartThreshold is the ramp's memory of the last 429: half the
	// batch size that drew it (at least the first batch). The next ramp
	// doubles only up to it and grows by one batch member per batch from
	// there (TCP slow start to ssthresh, then additive increase). Zero until
	// a 429 has interrupted a ramp.
	SlowStartThreshold int
	// Generation counts the block activations of this scope in this run. A
	// trial or probation member carries the generation it was released
	// under, so an outcome from before a re-block is recognized as stale.
	Generation uint64
}

func activeScopeFromBlockScopeRow(row *BlockScope) activeScope {
	if row == nil {
		return activeScope{}
	}

	return activeScope{
		Key:                row.Key,
		TrialInterval:      row.TrialInterval,
		NextTrialAt:        row.NextTrialAt,
		SlowStartThreshold: row.SlowStartThreshold,
	}
}

func blockScopeRowFromActiveScope(scope activeScope) (*BlockScope, error) {
	if describeScopeKey(scope.Key).IsZero() {
		return nil, fmt.Errorf("sync: unknown scope key %q", scope.Key.String())
	}
	if !scope.Key.PersistsInBlockScopes() {
		return nil, fmt.Errorf("sync: scope key %q is not a timed block scope", scope.Key.String())
	}

	return &BlockScope{
		Key:                scope.Key,
		TrialInterval:      scope.TrialInterval,
		NextTrialAt:        scope.NextTrialAt,
		SlowStartThreshold: scope.SlowStartThreshold,
	}, nil
}

// scopeUpdateResult describes the outcome of UpdateScope: whether a new scope
// block should be created. Does NOT contain the computed trial interval —
// interval computation is centralized in scope lifecycle policy helpers to
// prevent divergence between initial block creation and subsequent trial
// extensions.
type scopeUpdateResult struct {
	Block         bool          // true if threshold crossed → create block
	ScopeKey      ScopeKey      // scope key for the block
	ConditionType string        // "rate_limited", IssueQuotaExceeded, IssueServiceOutage
	RetryAfter    time.Duration // server-provided Retry-After (0 if absent)
}
