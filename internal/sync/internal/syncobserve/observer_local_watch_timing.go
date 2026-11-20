package syncobserve

import (
	"time"
)

const (
	LocalWatchHealthySafetyScanInterval = 5 * time.Minute
	// While watching is unavailable, establishing it is retried after
	// LocalWatchRetryInitialDelay, doubling to LocalWatchRetryMaxDelay.
	LocalWatchRetryInitialDelay = 10 * time.Minute
	LocalWatchRetryMaxDelay     = time.Hour
)
