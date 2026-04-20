package sync

import "time"

// BlockScope represents an active scope-level block (e.g., quota exceeded,
// service outage, rate limited) as persisted in block_scopes for restart,
// status, and other read-only projections.
type BlockScope struct {
	Key           ScopeKey          // typed scope key
	ConditionType string            // "service_outage", "quota_exceeded", "rate_limited"
	TimingSource  ScopeTimingSource // none, backoff, server_retry_after
	Family        ScopeFamily
	Access        ScopeAccess
	SubjectKind   ScopeSubjectKind
	SubjectValue  string

	BlockedAt     time.Time     // when the block was created
	TrialInterval time.Duration // current interval between trial actions (grows with backoff)
	NextTrialAt   time.Time     // when to dispatch the next trial
	TrialCount    int           // consecutive failed trials (for backoff)
}

// CoveredPath returns the validated scope path persisted on the block-scope row
// when available. Falling back to the key-derived descriptor keeps test-built
// or in-memory rows safe without making reparsing the normal persisted path.
func (block *BlockScope) CoveredPath() string {
	if block == nil {
		return ""
	}
	if block.SubjectKind != "" || block.SubjectValue != "" || block.Family != "" || block.Access != "" {
		return block.SubjectValue
	}

	return DescribeScopeKey(block.Key).ScopePath()
}

// ActiveScope is the watch-runtime working-set shape. It keeps only the
// mutable timer/admission facts needed for in-memory scope ownership; the
// persisted scope row remains the durable/read-side shape.
type ActiveScope struct {
	Key           ScopeKey
	TimingSource  ScopeTimingSource
	BlockedAt     time.Time
	TrialInterval time.Duration
	NextTrialAt   time.Time
	TrialCount    int
}

func activeScopeFromBlockScopeRow(row *BlockScope) ActiveScope {
	if row == nil {
		return ActiveScope{}
	}

	return ActiveScope{
		Key:           row.Key,
		TimingSource:  row.TimingSource,
		BlockedAt:     row.BlockedAt,
		TrialInterval: row.TrialInterval,
		NextTrialAt:   row.NextTrialAt,
		TrialCount:    row.TrialCount,
	}
}

func blockScopeRowFromActiveScope(scope ActiveScope) (*BlockScope, error) {
	metadata, err := encodePersistedScopeMetadata(scope.Key)
	if err != nil {
		return nil, err
	}

	return newBlockScopeFromPersistedMetadata(
		&metadata,
		metadata.Descriptor.DefaultConditionType,
		scope.TimingSource,
		scope.BlockedAt,
		scope.TrialInterval,
		scope.NextTrialAt,
		scope.TrialCount,
	), nil
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
