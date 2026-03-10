package retry

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Validates: R-6.8.8
func TestSyncTransport_MaxAttemptsZero(t *testing.T) {
	// SyncTransport is used by sync action dispatch. MaxAttempts=0 means the
	// graph client retry loop (attempt < MaxAttempts) executes zero iterations —
	// each dispatch is a single HTTP request. Failed actions return to the
	// tracker for re-queue with backoff instead of blocking on client-side retry.
	assert.Equal(t, 0, SyncTransport.MaxAttempts,
		"SyncTransport must have MaxAttempts=0 for single-attempt dispatch")
}
