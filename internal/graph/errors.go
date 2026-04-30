// Package graph provides an HTTP client for the Microsoft Graph API
// with automatic retry, rate limiting, and error classification.
package graph

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Sentinel errors for HTTP status code classification.
// Use errors.Is(err, graph.ErrNotFound) to check.
var (
	ErrBadRequest          = errors.New("graph: bad request")
	ErrUnauthorized        = errors.New("graph: unauthorized")
	ErrForbidden           = errors.New("graph: forbidden")
	ErrNotFound            = errors.New("graph: not found")
	ErrConflict            = errors.New("graph: conflict")
	ErrGone                = errors.New("graph: resource gone")
	ErrPreconditionFailed  = errors.New("graph: precondition failed")
	ErrThrottled           = errors.New("graph: throttled")
	ErrLocked              = errors.New("graph: resource locked")
	ErrMethodNotAllowed    = errors.New("graph: method not allowed")
	ErrRangeNotSatisfiable = errors.New("graph: range not satisfiable")
	ErrServerError         = errors.New("graph: server error")
	ErrNotLoggedIn         = errors.New("graph: not logged in")
)

// GraphError wraps a sentinel error with HTTP status code, request ID,
// and the API error message body for debugging.
type GraphError struct {
	StatusCode int
	RequestID  string
	Code       string
	InnerCodes []string
	Message    string
	RawBody    string
	Err        error // sentinel, for errors.Is()
	// RetryAfter is the server-mandated wait duration from the Retry-After
	// header on 429/503 responses. Zero when absent or unparseable. The engine
	// uses this for scope-based backoff instead of computing its own delay.
	RetryAfter time.Duration
}

// QuirkRetryAttempt records the observable evidence from one retryable Graph
// quirk attempt. It is attached to QuirkRetryError so callers can log or report
// the exact request IDs and Graph codes that exhausted the bounded quirk budget.
type QuirkRetryAttempt struct {
	Attempt    int    `json:"attempt"`
	StatusCode int    `json:"statusCode,omitempty"`
	GraphCode  string `json:"graphCode,omitempty"`
	RequestID  string `json:"requestId,omitempty"`
}

// QuirkEvidence is the narrow reusable projection of bounded documented-quirk
// retry evidence. It intentionally carries facts only; behavior and unwrapping
// remain on QuirkRetryError.
type QuirkEvidence struct {
	Quirk    string              `json:"quirk"`
	Attempts []QuirkRetryAttempt `json:"attempts,omitempty"`
}

// QuirkRetryError wraps the terminal error returned after a bounded documented
// Graph quirk retry budget is exhausted. It preserves the original cause for
// errors.Is / errors.As while exposing the retry evidence for callers that need
// richer degraded-mode logging or incident reporting.
type QuirkRetryError struct {
	Quirk    string              `json:"quirk"`
	Attempts []QuirkRetryAttempt `json:"attempts,omitempty"`
	Err      error               `json:"-"`
}

func (e *QuirkRetryError) Error() string {
	if e == nil {
		return "<nil>"
	}

	if e.Err == nil {
		return fmt.Sprintf("graph: %s retry exhausted after %d attempts", e.Quirk, len(e.Attempts))
	}

	return fmt.Sprintf("graph: %s retry exhausted after %d attempts: %v", e.Quirk, len(e.Attempts), e.Err)
}

func (e *QuirkRetryError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.Err
}

// ExtractQuirkEvidence projects structured retry evidence from a wrapped
// QuirkRetryError without asking callers to depend on the behavior-bearing
// error type directly.
func ExtractQuirkEvidence(err error) (QuirkEvidence, bool) {
	var quirkErr *QuirkRetryError
	if !errors.As(err, &quirkErr) || quirkErr == nil {
		return QuirkEvidence{}, false
	}

	return QuirkEvidence{
		Quirk:    quirkErr.Quirk,
		Attempts: append([]QuirkRetryAttempt(nil), quirkErr.Attempts...),
	}, true
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

func (e *GraphError) MostSpecificCode() string {
	for i := len(e.InnerCodes) - 1; i >= 0; i-- {
		if e.InnerCodes[i] != "" {
			return e.InnerCodes[i]
		}
	}

	return e.Code
}

func (e *GraphError) HasCode(code string) bool {
	if strings.EqualFold(e.Code, code) {
		return true
	}

	for i := range e.InnerCodes {
		if strings.EqualFold(e.InnerCodes[i], code) {
			return true
		}
	}

	return false
}

type graphErrorEnvelope struct {
	Error graphErrorNode `json:"error"`
}

type graphErrorNode struct {
	Code            string          `json:"code"`
	Message         string          `json:"message"`
	InnerError      json.RawMessage `json:"innerError"`
	InnerErrorLower json.RawMessage `json:"innererror"`
}

func parseGraphErrorBody(body []byte) (code, message string, innerCodes []string) {
	var envelope graphErrorEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", "", nil
	}

	code = envelope.Error.Code
	message = envelope.Error.Message
	innerCodes = parseInnerErrorCodes(firstNonEmptyRawMessage(
		envelope.Error.InnerError,
		envelope.Error.InnerErrorLower,
	))

	if code == "" && message == "" && len(innerCodes) == 0 {
		return "", "", nil
	}

	return code, message, innerCodes
}

func parseInnerErrorCodes(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}

	var node graphErrorNode
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil
	}

	var codes []string
	if node.Code != "" {
		codes = append(codes, node.Code)
	}

	return append(codes, parseInnerErrorCodes(firstNonEmptyRawMessage(node.InnerError, node.InnerErrorLower))...)
}

func firstNonEmptyRawMessage(values ...json.RawMessage) json.RawMessage {
	for i := range values {
		if len(values[i]) > 0 {
			return values[i]
		}
	}

	return nil
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
	case http.StatusPreconditionFailed:
		return ErrPreconditionFailed
	case http.StatusTooManyRequests:
		return ErrThrottled
	case http.StatusMethodNotAllowed:
		return ErrMethodNotAllowed
	case http.StatusLocked:
		return ErrLocked
	case http.StatusRequestedRangeNotSatisfiable:
		return ErrRangeNotSatisfiable
	default:
		if code >= http.StatusInternalServerError {
			return ErrServerError
		}

		return nil
	}
}
