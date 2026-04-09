package retry

import "time"

// Canonical retry constants. Kept local to this package so callers consume the
// fully shaped Policy values rather than piecing them together themselves.
const (
	transportAttempts            = 5
	driveDiscoveryAttempts       = 5
	rootChildrenAttempts         = 3
	downloadMetadataAttempts     = 4
	simpleUploadMtimeAttempts    = 4
	uploadSessionCreateAttempts  = 6
	simpleUploadCreateAttempts   = 7
	pathVisibilityAttempts       = 10
	infiniteAttempts             = 0
	standardMultiplier           = 2.0
	standardJitter               = 0.25
	noJitter                     = 0.0
	defaultBaseDelay             = 1 * time.Second
	rootChildrenBaseDelay        = 250 * time.Millisecond
	rootChildrenMaxDelay         = 1 * time.Second
	downloadMetadataBaseDelay    = 250 * time.Millisecond
	downloadMetadataMaxDelay     = 2 * time.Second
	simpleUploadMtimeBaseDelay   = 250 * time.Millisecond
	simpleUploadMtimeMaxDelay    = 2 * time.Second
	uploadSessionCreateBaseDelay = 250 * time.Millisecond
	uploadSessionCreateMaxDelay  = 4 * time.Second
	simpleUploadCreateBaseDelay  = 250 * time.Millisecond
	simpleUploadCreateMaxDelay   = 8 * time.Second
	pathVisibilityBaseDelay      = 250 * time.Millisecond
	pathVisibilityMaxDelay       = 32 * time.Second
	transportMaxDelay            = 60 * time.Second
	watchLocalMaxDelay           = 30 * time.Second
	watchRemoteBaseDelay         = 5 * time.Second
	watchRemoteMaxDelay          = 5 * time.Minute
)

// TransportPolicy is the HTTP transport retry policy (graph/client.go).
// 5 retries, 1s base, 60s max, 2x multiplier, ±25% jitter.
func TransportPolicy() Policy {
	return Policy{
		MaxAttempts: transportAttempts,
		Base:        defaultBaseDelay,
		Max:         transportMaxDelay,
		Multiplier:  standardMultiplier,
		Jitter:      standardJitter,
	}
}

// DriveDiscoveryPolicy is the transient-403 retry policy for drive
// enumeration (graph/drives.go). 5 attempts, same backoff curve as Transport.
func DriveDiscoveryPolicy() Policy {
	return Policy{
		MaxAttempts: driveDiscoveryAttempts,
		Base:        defaultBaseDelay,
		Max:         transportMaxDelay,
		Multiplier:  standardMultiplier,
		Jitter:      standardJitter,
	}
}

// RootChildrenPolicy is the quick-retry policy for the documented transient
// 404 misfire on GET /drives/{id}/items/root/children. It is intentionally
// shorter than drive discovery because root listing is a hot path for both CLI
// commands and sync observation.
func RootChildrenPolicy() Policy {
	return Policy{
		MaxAttempts: rootChildrenAttempts,
		Base:        rootChildrenBaseDelay,
		Max:         rootChildrenMaxDelay,
		Multiplier:  standardMultiplier,
		Jitter:      noJitter,
	}
}

// DownloadMetadataPolicy is the quick-retry policy for transient item-not-found
// misfires when download flows fetch item metadata by ID to obtain the
// pre-authenticated download URL. It stays short because ordinary file downloads
// should not block for long, but it is slightly longer than root-children
// because the object often becomes readable within the next second or two.
func DownloadMetadataPolicy() Policy {
	return Policy{
		MaxAttempts: downloadMetadataAttempts,
		Base:        downloadMetadataBaseDelay,
		Max:         downloadMetadataMaxDelay,
		Multiplier:  standardMultiplier,
		Jitter:      noJitter,
	}
}

// SimpleUploadMtimePatchPolicy is the quick-retry policy for transient
// item-not-found misfires when the immediate post-simple-upload
// UpdateFileSystemInfo PATCH races Graph's item-by-ID visibility.
func SimpleUploadMtimePatchPolicy() Policy {
	return Policy{
		MaxAttempts: simpleUploadMtimeAttempts,
		Base:        simpleUploadMtimeBaseDelay,
		Max:         simpleUploadMtimeMaxDelay,
		Multiplier:  standardMultiplier,
		Jitter:      noJitter,
	}
}

// UploadSessionCreatePolicy is the quick-retry policy for transient
// item-not-found misfires when createUploadSession targets a freshly created
// parent folder. The budget is long enough to bridge the live consistency
// window we have observed after freshly created folders become path-visible,
// while still staying bounded for real missing parents or permission problems.
func UploadSessionCreatePolicy() Policy {
	return Policy{
		MaxAttempts: uploadSessionCreateAttempts,
		Base:        uploadSessionCreateBaseDelay,
		Max:         uploadSessionCreateMaxDelay,
		Multiplier:  standardMultiplier,
		Jitter:      noJitter,
	}
}

// SimpleUploadCreatePolicy is the longer post-disambiguation retry policy for
// create-by-parent simple uploads that still return item-not-found after the
// session-route permission oracle has already exhausted on the same parent.
func SimpleUploadCreatePolicy() Policy {
	return Policy{
		MaxAttempts: simpleUploadCreateAttempts,
		Base:        simpleUploadCreateBaseDelay,
		Max:         simpleUploadCreateMaxDelay,
		Multiplier:  standardMultiplier,
		Jitter:      noJitter,
	}
}

// PathVisibilityPolicy is the bounded post-mutation visibility wait for
// ordinary path reads after Graph has already acknowledged a create, upload, or
// move. The schedule is intentionally deterministic because callers use it as a
// user-facing readiness gate rather than a best-effort background retry.
func PathVisibilityPolicy() Policy {
	return Policy{
		MaxAttempts: pathVisibilityAttempts,
		Base:        pathVisibilityBaseDelay,
		Max:         pathVisibilityMaxDelay,
		Multiplier:  standardMultiplier,
		Jitter:      noJitter,
	}
}

// WatchLocalPolicy is the local observer error backoff policy
// (observer_local.go). Infinite attempts (watch loop), 1s base, 30s max,
// 2x multiplier, no jitter.
func WatchLocalPolicy() Policy {
	return Policy{
		MaxAttempts: infiniteAttempts,
		Base:        defaultBaseDelay,
		Max:         watchLocalMaxDelay,
		Multiplier:  standardMultiplier,
		Jitter:      noJitter,
	}
}

// ReconcilePolicy is the single retry curve for all sync failures
// (sync_failures table + engine failure retrier). Infinite attempts until the
// failure succeeds or becomes actionable. Curve: 1s, 2s, 4s, 8s, 16s, 32s,
// 64s, 128s, 256s, 512s, 1024s, 2048s, 3600s cap.
func ReconcilePolicy() Policy {
	return Policy{
		MaxAttempts: infiniteAttempts,
		Base:        defaultBaseDelay,
		Max:         time.Hour,
		Multiplier:  standardMultiplier,
		Jitter:      standardJitter,
	}
}

// WatchRemotePolicy is the remote observer error backoff policy
// (observer_remote.go). Infinite attempts, 5s base, dynamic max (poll
// interval), 2x multiplier, no jitter. The actual max is set dynamically via
// Backoff.SetMaxOverride().
func WatchRemotePolicy() Policy {
	return Policy{
		MaxAttempts: infiniteAttempts,
		Base:        watchRemoteBaseDelay,
		Max:         watchRemoteMaxDelay,
		Multiplier:  standardMultiplier,
		Jitter:      noJitter,
	}
}
