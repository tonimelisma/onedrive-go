package retry

import (
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

// WatchLocal is the local observer error backoff policy (observer_local.go).
// Infinite attempts (watch loop), 1s base, 30s max, 2x multiplier, no jitter.
var WatchLocal = Policy{ //nolint:gochecknoglobals // named policy singleton
	MaxAttempts: 0, // infinite
	Base:        1 * time.Second,
	Max:         30 * time.Second, //nolint:mnd // canonical max backoff
	Multiplier:  2.0,              //nolint:mnd // standard exponential factor
	Jitter:      0.0,
}

// Reconcile is the single retry curve for all sync failures (sync_failures + FailureRetrier).
// Infinite attempts (reconciler runs until success or the failure becomes actionable).
// Curve: 1s, 2s, 4s, 8s, 16s, 32s, 64s, 128s, 256s, 512s, 1024s, 2048s, 3600s cap.
var Reconcile = Policy{ //nolint:gochecknoglobals // named policy singleton
	MaxAttempts: 0, // infinite — reconciler retries until success or actionable
	Base:        1 * time.Second,
	Max:         time.Hour,
	Multiplier:  2.0,  //nolint:mnd // standard exponential factor
	Jitter:      0.25, //nolint:mnd // ±25% jitter fraction
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
