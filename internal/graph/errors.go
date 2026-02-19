// Package graph provides an HTTP client for the Microsoft Graph API
// with automatic retry, rate limiting, and error classification.
package graph

import (
	"errors"
	"fmt"
	"net/http"
)

// Sentinel errors for HTTP status code classification.
// Use errors.Is(err, graph.ErrNotFound) to check.
var (
	ErrBadRequest   = errors.New("graph: bad request")
	ErrUnauthorized = errors.New("graph: unauthorized")
	ErrForbidden    = errors.New("graph: forbidden")
	ErrNotFound     = errors.New("graph: not found")
	ErrConflict     = errors.New("graph: conflict")
	ErrGone         = errors.New("graph: resource gone")
	ErrThrottled    = errors.New("graph: throttled")
	ErrLocked       = errors.New("graph: resource locked")
	ErrServerError  = errors.New("graph: server error")
)

// GraphError wraps a sentinel error with HTTP status code, request ID,
// and the API error message body for debugging.
type GraphError struct {
	StatusCode int
	RequestID  string
	Message    string
	Err        error // sentinel, for errors.Is()
}

func (e *GraphError) Error() string {
	if e.RequestID != "" {
		return fmt.Sprintf("graph: HTTP %d (request-id: %s): %s", e.StatusCode, e.RequestID, e.Message)
	}

	return fmt.Sprintf("graph: HTTP %d: %s", e.StatusCode, e.Message)
}

func (e *GraphError) Unwrap() error {
	return e.Err
}

// classifyStatus maps an HTTP status code to a sentinel error.
// Returns nil for 2xx success codes.
func classifyStatus(code int) error {
	switch code {
	case http.StatusBadRequest:
		return ErrBadRequest
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusForbidden:
		return ErrForbidden
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusConflict:
		return ErrConflict
	case http.StatusGone:
		return ErrGone
	case http.StatusTooManyRequests:
		return ErrThrottled
	case http.StatusLocked:
		return ErrLocked
	default:
		if code >= http.StatusInternalServerError {
			return ErrServerError
		}

		return nil
	}
}

// isRetryable reports whether the given HTTP status code should be retried.
func isRetryable(code int) bool {
	switch code {
	case http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		// 509 Bandwidth Limit Exceeded (SharePoint).
		const statusBandwidthExceeded = 509
		return code == statusBandwidthExceeded
	}
}
