package sync

import "time"

// BlockScope represents an active scope-level block (e.g., quota exceeded,
// service outage, rate limited). While a block is active, all actions matching
// that scope are deferred via blocked retry_work rows.
type BlockScope struct {
	Key          ScopeKey          // typed scope key
	IssueType    string            // "service_outage", "quota_exceeded", "rate_limited"
	TimingSource ScopeTimingSource // none, backoff, server_retry_after

	BlockedAt     time.Time     // when the block was created
	TrialInterval time.Duration // current interval between trial actions (grows with backoff)
	NextTrialAt   time.Time     // when to dispatch the next trial
	TrialCount    int           // consecutive failed trials (for backoff)
}

// ScopeUpdateResult describes the outcome of UpdateScope: whether a new scope
// block should be created. Does NOT contain the computed trial interval —
// interval computation is centralized in computeTrialInterval() to prevent
// divergence between initial block creation and subsequent trial extensions.
type ScopeUpdateResult struct {
	Block      bool          // true if threshold crossed → create block
	ScopeKey   ScopeKey      // scope key for the block
	IssueType  string        // "rate_limited", IssueQuotaExceeded, IssueServiceOutage
	RetryAfter time.Duration // server-provided Retry-After (0 if absent)
}
