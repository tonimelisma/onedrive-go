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
	// ErrLoginExpired marks a saved refresh token Microsoft no longer accepts
	// (OAuth invalid_grant: expired after inactivity, revoked, or password
	// changed). It always travels together with ErrUnauthorized.
	ErrLoginExpired = errors.New("graph: saved login expired")
	// ErrLoginDeclined marks an interactive sign-in the user canceled or
	// declined (OAuth access_denied / authorization_declined). Its text is
	// the user-facing message; the raw OAuth detail is logged at Debug.
	ErrLoginDeclined = errors.New("sign-in was canceled or declined")
	// ErrLoginCodeExpired marks a device-code sign-in whose code expired
	// before the user entered it (OAuth expired_token).
	ErrLoginCodeExpired = errors.New("the sign-in code expired before it was entered")

	// ErrTimestampOutOfRange marks a fileSystemInfo timestamp OneDrive does
	// not accept. Personal refuses anything before 1900-01-01T00:00:00Z with a
	// misleading 404 itemNotFound (live probe L-26, 2026-10-25); the client
	// re-labels that 404 so nothing downstream reads it as "the item is gone"
	// and forgets a live item.
	ErrTimestampOutOfRange = errors.New("graph: modification time out of OneDrive's range")

	// ErrProviderReadOnly marks a Microsoft-side read-only window: the service
	// answers, authentication is valid, and the account is fine, but the
	// backing store will not serve the request. It is reported as an outer
	// accessDenied with an inner serviceReadOnly, which is indistinguishable
	// from an ordinary permission failure unless the inner code is inspected.
	//
	// It is deliberately separate from ErrForbidden. Forbidden means "this
	// identity may not do that" and is a stable answer; a read-only window is a
	// temporary property of the service that no client action changes.
	ErrProviderReadOnly = errors.New("graph: provider is in a read-only window")
)

// graphCodeServiceReadOnly is the inner Graph code for a backend read-only
// window.
const graphCodeServiceReadOnly = "serviceReadOnly"

// earliestFileSystemYear is the first year OneDrive Personal accepts in a
// fileSystemInfo timestamp (live probe L-26): 1899-12-31T23:59:59Z returns
// 404, 1900-01-01T00:00:00Z is stored.
const earliestFileSystemYear = 1900

// EarliestFileSystemTime is the earliest fileSystemInfo timestamp OneDrive
// accepts.
func EarliestFileSystemTime() time.Time {
	return time.Date(earliestFileSystemYear, time.January, 1, 0, 0, 0, 0, time.UTC)
}

// relabelTimestampRangeError turns the 404 itemNotFound OneDrive returns for a
// pre-1900 fileSystemInfo timestamp into ErrTimestampOutOfRange, keeping the
// status, request ID and body for diagnosis. Any other error is unchanged.
func relabelTimestampRangeError(err error, sent time.Time) error {
	if !sent.Before(EarliestFileSystemTime()) || !errors.Is(err, ErrNotFound) {
		return err
	}

	var graphErr *GraphError
	if !errors.As(err, &graphErr) {
		return fmt.Errorf("%w (sent %s): %w", ErrTimestampOutOfRange, sent.UTC().Format(time.RFC3339), err)
	}

	relabeled := *graphErr
	relabeled.Err = ErrTimestampOutOfRange
	relabeled.Message = fmt.Sprintf("modification time %s is before %s, which OneDrive does not accept (it answers %q)",
		sent.UTC().Format(time.RFC3339), EarliestFileSystemTime().Format(time.RFC3339), graphErr.Message)

	return &relabeled
}

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
	// AgsDiagnostic is the X-Ms-Ags-Diagnostic response header: the Graph
	// gateway's routing record (server, data center). Microsoft support asks
	// for it next to the request ID when an outage or throttle is escalated.
	AgsDiagnostic string
}

// QuirkRetryAttempt records the observable evidence from one retryable Graph
// quirk attempt. It is attached to QuirkRetryError so callers can log or report
// the exact request IDs and Graph codes that exhausted the bounded quirk budget.
type QuirkRetryAttempt struct {
	Attempt       int    `json:"attempt"`
	StatusCode    int    `json:"statusCode,omitempty"`
	GraphCode     string `json:"graphCode,omitempty"`
	RequestID     string `json:"requestId,omitempty"`
	AgsDiagnostic string `json:"agsDiagnostic,omitempty"`
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

// Error says, in plain words, what OneDrive kept doing. The quirk name is
// not part of the text: it is on Quirk (and the QuirkLogKey log field) for
// anything that needs to recognize the quirk.
func (e *QuirkRetryError) Error() string {
	if e == nil {
		return "<nil>"
	}

	text := fmt.Sprintf("graph: OneDrive %s, even after %d tries", quirkSymptom(e.Quirk), len(e.Attempts))
	if e.Err == nil {
		return text
	}

	return text + ": " + e.Err.Error()
}

// quirkSymptom describes what a documented quirk looks like from outside.
func quirkSymptom(quirk string) string {
	switch quirk {
	case QuirkDrivesTokenPropagation:
		return "did not yet accept the new sign-in"
	case QuirkCopyDestinationTransient404:
		return "kept reporting the copy's destination folder as missing"
	case QuirkUploadSessionCreateTransient404, QuirkSimpleUploadCreateTransient404:
		return "kept reporting the upload's destination folder as missing"
	case QuirkSimpleUploadMtimeTransient404:
		return "kept reporting the just-uploaded file as missing while setting its modified time"
	case QuirkDownloadMetadataTransient404:
		return "kept reporting the file as missing when starting the download"
	case QuirkRootChildrenTransient404:
		return "kept reporting the drive's top folder as missing"
	default:
		return "kept failing with a known temporary error"
	}
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

// Is recognizes ErrProviderReadOnly from the inner Graph code so callers can
// branch on provider unavailability without string-matching codes themselves.
// Returning false for other targets lets Unwrap continue to the transport
// sentinel.
func (e *GraphError) Is(target error) bool {
	if errors.Is(target, ErrProviderReadOnly) {
		return e.HasCode(graphCodeServiceReadOnly)
	}

	return false
}

// Graph codes and message fragments that identify a full OneDrive or a
// refused operation regardless of the HTTP status they arrive under.
const (
	graphCodeQuotaLimitReached    = "quotaLimitReached"
	graphCodeInsufficientStorage  = "insufficientStorage"
	graphCodeNotSupported         = "notSupported"
	graphMessageInsufficientSpace = "insufficient space"
)

// IsQuotaExhausted reports whether err is OneDrive saying the drive is full.
// 507 is the documented answer, but OneDrive also reports a full drive as 403
// quotaLimitReached and (on Business) as 500 "Insufficient Space Available",
// so the most specific code and message are checked before the status.
func IsQuotaExhausted(err error) bool {
	var graphErr *GraphError
	if !errors.As(err, &graphErr) {
		return false
	}

	return graphErr.StatusCode == http.StatusInsufficientStorage ||
		graphErr.HasCode(graphCodeQuotaLimitReached) ||
		graphErr.HasCode(graphCodeInsufficientStorage) ||
		strings.Contains(strings.ToLower(graphErr.Message), graphMessageInsufficientSpace)
}

// IsNotSupported reports whether err is OneDrive refusing an operation it
// does not implement for this item: 501, 505, or a notSupported code under
// any status. It is a stable answer about one request, never an outage.
func IsNotSupported(err error) bool {
	var graphErr *GraphError
	if !errors.As(err, &graphErr) {
		return false
	}

	return graphErr.StatusCode == http.StatusNotImplemented ||
		graphErr.StatusCode == http.StatusHTTPVersionNotSupported ||
		graphErr.HasCode(graphCodeNotSupported)
}

// IsProviderUnavailable reports whether err is a provider-side condition that
// no client retry or configuration change can clear. Callers use it to
// distinguish "we could not test/serve because the provider is down" from "the
// request was wrong".
func IsProviderUnavailable(err error) bool {
	return errors.Is(err, ErrProviderReadOnly)
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

// MostSpecificErrorCode returns the innermost Graph error code in a raw error
// response body, falling back to the outer code. Callers outside the client --
// live test preflights in particular -- need the same view of a response the
// client has, and the distinction matters: a backend read-only window is an
// outer accessDenied whose inner code is the only thing that identifies it.
func MostSpecificErrorCode(body []byte) string {
	code, _, innerCodes := parseGraphErrorBody(body)
	for i := len(innerCodes) - 1; i >= 0; i-- {
		if innerCodes[i] != "" {
			return innerCodes[i]
		}
	}

	return code
}

// IsProviderReadOnlyCode reports whether a Graph error code identifies a
// backend read-only window.
func IsProviderReadOnlyCode(code string) bool {
	return strings.EqualFold(code, graphCodeServiceReadOnly)
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
