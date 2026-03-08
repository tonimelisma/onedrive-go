package retry

import (
	"math"
	"time"
)

// Named policy instances. Each captures the exact parameters from the original
// implementation it replaces, preserving identical behavior. These numbers are
// the canonical specification — they originate here and are referenced elsewhere.

// Transport is the HTTP transport retry policy (graph/client.go).
// 5 retries, 1s base, 60s max, 2x multiplier, ±25% jitter.
var Transport = Policy{ //nolint:gochecknoglobals // named policy singleton
	MaxAttempts: 5, //nolint:mnd // canonical retry count
	Base:        1 * time.Second,
	Max:         60 * time.Second, //nolint:mnd // canonical max backoff
	Multiplier:  2.0,              //nolint:mnd // standard exponential factor
	Jitter:      0.25,             //nolint:mnd // ±25% jitter fraction
}

// DriveDiscovery is the transient-403 retry policy for drive enumeration
// (graph/drives.go). 3 attempts, same backoff curve as Transport.
var DriveDiscovery = Policy{ //nolint:gochecknoglobals // named policy singleton
	MaxAttempts: 3,
	Base:        1 * time.Second,
	Max:         60 * time.Second, //nolint:mnd // canonical max backoff
	Multiplier:  2.0,              //nolint:mnd // standard exponential factor
	Jitter:      0.25,             //nolint:mnd // ±25% jitter fraction
}

// Action is the executor-level retry policy (sync/executor.go).
// 3 retries, 1s base, no explicit max (grows unbounded in 3 attempts:
// 1s, 2s, 4s — never reaches any reasonable cap).
var Action = Policy{ //nolint:gochecknoglobals // named policy singleton
	MaxAttempts: 3,
	Base:        1 * time.Second,
	Max:         time.Duration(math.MaxInt64), // effectively unbounded for 3 attempts
	Multiplier:  2.0,                          //nolint:mnd // standard exponential factor
	Jitter:      0.25,                         //nolint:mnd // ±25% jitter fraction
}

// Reconcile is the failure retrier scheduling policy (sync/baseline.go).
// Used to compute next_retry_at for failed items. 10 attempts (escalation
// threshold), 30s base, 1h max, 2x multiplier, ~25% jitter.
var Reconcile = Policy{ //nolint:gochecknoglobals // named policy singleton
	MaxAttempts: 10,               //nolint:mnd // escalation threshold
	Base:        30 * time.Second, //nolint:mnd // canonical base backoff
	Max:         1 * time.Hour,
	Multiplier:  2.0,  //nolint:mnd // standard exponential factor
	Jitter:      0.25, //nolint:mnd // ±25% jitter fraction
}

// WatchLocal is the local observer error backoff policy (observer_local.go).
// Infinite attempts (watch loop), 1s base, 30s max, 2x multiplier, no jitter.
var WatchLocal = Policy{ //nolint:gochecknoglobals // named policy singleton
	MaxAttempts: 0, // infinite
	Base:        1 * time.Second,
	Max:         30 * time.Second, //nolint:mnd // canonical max backoff
	Multiplier:  2.0,              //nolint:mnd // standard exponential factor
	Jitter:      0.0,
}

// WatchRemote is the remote observer error backoff policy (observer_remote.go).
// Infinite attempts, 5s base, dynamic max (poll interval), 2x multiplier, no jitter.
// The actual max is set dynamically via Backoff.SetMaxOverride().
var WatchRemote = Policy{ //nolint:gochecknoglobals // named policy singleton
	MaxAttempts: 0,               // infinite
	Base:        5 * time.Second, //nolint:mnd // canonical base backoff
	Max:         5 * time.Minute, //nolint:mnd // default ceiling; overridden by SetMaxOverride
	Multiplier:  2.0,             //nolint:mnd // standard exponential factor
	Jitter:      0.0,
}
