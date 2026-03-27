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
