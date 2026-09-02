package sync

import (
	"errors"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// These decode a Graph transport error into the two facts the domain records
// on a completed action: the HTTP status and any server-supplied retry delay.
// They sit in the domain because worker_result.go builds that record; leaving
// them in the worker made the domain appear to depend on execution machinery
// when it depends only on how a transport failure is described.

// extractHTTPStatus unwraps a graph.GraphError from err and returns its
// StatusCode. Returns 0 if err is nil or not a GraphError.
func extractHTTPStatus(err error) int {
	if err == nil {
		return 0
	}

	var ge *graph.GraphError
	if errors.As(err, &ge) {
		return ge.StatusCode
	}

	return 0
}

// extractRetryAfter unwraps a graph.GraphError from err and returns its
// RetryAfter duration. Returns 0 if err is nil or not a GraphError.
func extractRetryAfter(err error) time.Duration {
	if err == nil {
		return 0
	}

	var ge *graph.GraphError
	if errors.As(err, &ge) {
		return ge.RetryAfter
	}

	return 0
}
