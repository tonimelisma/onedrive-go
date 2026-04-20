package sync

import (
	"fmt"
	"time"
)

func isTimedBlockScopeKey(key ScopeKey) bool {
	if key.IsZero() {
		return false
	}
	if key.IsPermLocalRead() || key.IsPermRemoteRead() {
		return false
	}
	return !DescribeScopeKey(key).IsZero()
}

// BlockScope represents one active timed shared blocker as persisted in
// block_scopes for restart, admission, and read-side projections.
type BlockScope struct {
	Key           ScopeKey
	BlockedAt     time.Time     // when the block was created
	TrialInterval time.Duration // current interval between trial actions
	NextTrialAt   time.Time     // when to dispatch the next trial
}

// CoveredPath returns the scope path encoded in the scope key when available.
func (block *BlockScope) CoveredPath() string {
	if block == nil {
		return ""
	}
	return DescribeScopeKey(block.Key).ScopePath()
}

// ActiveScope is the watch-runtime working-set shape. It keeps only the
// mutable timer/admission facts needed for in-memory scope ownership; the
// persisted scope row remains the durable/read-side shape.
type ActiveScope struct {
	Key           ScopeKey
	BlockedAt     time.Time
	TrialInterval time.Duration
	NextTrialAt   time.Time
}

func activeScopeFromBlockScopeRow(row *BlockScope) ActiveScope {
	if row == nil {
		return ActiveScope{}
	}

	return ActiveScope{
		Key:           row.Key,
		BlockedAt:     row.BlockedAt,
		TrialInterval: row.TrialInterval,
		NextTrialAt:   row.NextTrialAt,
	}
}

func blockScopeRowFromActiveScope(scope ActiveScope) (*BlockScope, error) {
	if DescribeScopeKey(scope.Key).IsZero() {
		return nil, fmt.Errorf("sync: unknown scope key %q", scope.Key.String())
	}
	if !isTimedBlockScopeKey(scope.Key) {
		return nil, fmt.Errorf("sync: scope key %q is not a timed block scope", scope.Key.String())
	}

	return &BlockScope{
		Key:           scope.Key,
		BlockedAt:     scope.BlockedAt,
		TrialInterval: scope.TrialInterval,
		NextTrialAt:   scope.NextTrialAt,
	}, nil
}

// ScopeUpdateResult describes the outcome of UpdateScope: whether a new scope
// block should be created. Does NOT contain the computed trial interval —
// interval computation is centralized in computeTrialInterval() to prevent
// divergence between initial block creation and subsequent trial extensions.
type ScopeUpdateResult struct {
	Block         bool          // true if threshold crossed → create block
	ScopeKey      ScopeKey      // scope key for the block
	ConditionType string        // "rate_limited", IssueQuotaExceeded, IssueServiceOutage
	RetryAfter    time.Duration // server-provided Retry-After (0 if absent)
}
