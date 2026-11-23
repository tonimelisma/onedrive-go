package syncdomain

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// These decode a Graph transport error into the two facts the domain records
// on a completed action: the HTTP status and any server-supplied retry delay.
// They sit in the domain because worker_result.go builds that record; leaving
// them in the worker made the domain appear to depend on execution machinery
// when it depends only on how a transport failure is described.

// ExtractHTTPStatus unwraps a graph.GraphError from err and returns its
// StatusCode. Returns 0 if err is nil or not a GraphError.
func ExtractHTTPStatus(err error) int {
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

// ServerAnswer is what a Graph failure means for the work around it. It is
// the one table from (HTTP status, Graph code, message) to meaning: scope
// detection, result classification and issue naming all read it, so a quota
// answer or a refusal is recognized identically everywhere. The most specific
// evidence wins: a Graph code or a known message is checked before the bare
// status, because OneDrive reports one condition under several statuses. A
// request OneDrive never answered (IsRequestTimeout) means what a 5xx means:
// the service is not answering.
type ServerAnswer int

const (
	ServerAnswerNone ServerAnswer = iota
	ServerAnswerUnauthorized
	ServerAnswerForbidden
	ServerAnswerThrottled
	// ServerAnswerQuota is a full OneDrive: 507, or quotaLimitReached /
	// insufficientStorage or an "insufficient space" message under any
	// status (Business has returned it as 500 and 403).
	ServerAnswerQuota
	ServerAnswerServiceOutage
	// ServerAnswerUnsupported is OneDrive refusing an operation it does not
	// implement (501, 505, or a notSupported code under any status). It is a
	// stable answer about one request, not an outage, so it must never count
	// toward the service scope: counted, five refusals would block every
	// drive.
	ServerAnswerUnsupported
	ServerAnswerPathTooLong
	ServerAnswerRequestTimeout
	// ServerAnswerTransientConflict is 412, and 409 resourceModified: the
	// item changed between our read and our write (an mtime PATCH racing a
	// server-side post-upload update), so a retry against fresh truth works.
	ServerAnswerTransientConflict
	ServerAnswerTransientNotFound
	ServerAnswerLocked
	// ServerAnswerTransferURLRejected is a pre-authenticated upload or
	// download URL refusing its own credential (graph.ErrPreAuthURLRejected).
	// Only that file's transfer is affected; it is retried with a new URL.
	ServerAnswerTransferURLRejected
	// ServerAnswerMalware is content OneDrive flagged as malware
	// (graph.ErrMalwareDetected). It is a permanent per-file issue: probed as
	// a permission problem, the download's 403 would be labeled read-denied
	// and retried forever.
	ServerAnswerMalware
	ServerAnswerOther
)

// Graph codes and message fragments the server-answer table recognizes
// beyond graph.IsQuotaExhausted and graph.IsNotSupported.
const (
	GraphCodeResourceModified = "resourceModified"
	graphMessageNameTooLong   = "name is too long"
	graphMessagePathLimit     = "400 characters or less"
)

// classifyServerAnswer maps one Graph failure to its meaning. status is the
// HTTP status recorded on the completion; err supplies the code and message
// when it carries a graph.GraphError.
func classifyServerAnswer(status int, err error) ServerAnswer {
	if errors.Is(err, graph.ErrMalwareDetected) {
		// Checked before the status: the facet check refuses before any
		// request, so there may be no status at all.
		return ServerAnswerMalware
	}
	if status == 0 {
		return unansweredServerAnswer(err)
	}

	var graphErr *graph.GraphError
	hasGraphErr := errors.As(err, &graphErr)
	message := ""
	if hasGraphErr {
		message = strings.ToLower(graphErr.Message)
	}

	switch {
	case errors.Is(err, graph.ErrTimestampOutOfRange):
		// The client clamps every time it sends to 1900 or later, so this
		// is a defense: a generic per-file failure, never "the item is
		// gone" (the wire status is a misleading 404).
		return ServerAnswerOther
	case errors.Is(err, graph.ErrAuthTransient):
		// A 401 that a just-refreshed token still got, naming a server-side
		// validation fault: an outage, never "sign-in required" (D-072).
		return ServerAnswerServiceOutage
	case errors.Is(err, graph.ErrPreAuthURLRejected):
		return ServerAnswerTransferURLRejected
	case status == http.StatusInsufficientStorage, graph.IsQuotaExhausted(err):
		return ServerAnswerQuota
	case status == http.StatusNotImplemented, status == http.StatusHTTPVersionNotSupported, graph.IsNotSupported(err):
		return ServerAnswerUnsupported
	case status == http.StatusBadRequest &&
		(strings.Contains(message, graphMessageNameTooLong) || strings.Contains(message, graphMessagePathLimit)):
		// OneDrive accepted a path below the drive root of 364 UTF-16 units
		// and refused one of 365 (L-36; why below its stated 400 is not
		// known), so the scanner's 400 cannot catch every case, and a bare
		// 400 would retry forever.
		return ServerAnswerPathTooLong
	case status == http.StatusConflict && hasGraphErr && graphErr.HasCode(GraphCodeResourceModified):
		return ServerAnswerTransientConflict
	}

	return classifyServerStatus(status)
}

// unansweredServerAnswer is the meaning of a failure that carries no status:
// a request that did not reach OneDrive or got no answer is the service's
// silence, the outage a 5xx says out loud; any other carries no answer.
func unansweredServerAnswer(err error) ServerAnswer {
	if IsServiceUnreached(err) {
		return ServerAnswerServiceOutage
	}

	return ServerAnswerNone
}

// IsRequestTimeout reports a request that ran out of time waiting for
// OneDrive, or for the Microsoft sign-in its token comes from. The dial,
// response-header and whole-request timeouts and an expired request deadline
// carry context.DeadlineExceeded; a transfer the stall watchdog abandoned
// carries graph.ErrTransferStalled instead, because the watchdog keeps its
// cancellation out of the chain; an answer whose body brought nothing for the
// stall window carries graph.ErrResponseStalled, and a request or body read
// on an HTTP/2 connection closed for an unanswered ping
// graph.ErrConnectionLost; a TLS handshake timeout and a read whose
// connection's keepalive found the server gone carry none of these
// (retry.IsNetworkTimeout). An outcome commit or created-item record that outlasted
// its bound carries context.DeadlineExceeded too, but it is no request:
// ErrStoreAccessTimedOut marks it. Whether the run had ended is not asked
// here: classifyResult asks.
func IsRequestTimeout(err error) bool {
	if errors.Is(err, ErrStoreAccessTimedOut) {
		return false
	}

	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, graph.ErrTransferStalled) ||
		errors.Is(err, graph.ErrResponseStalled) || errors.Is(err, graph.ErrConnectionLost) ||
		retry.IsNetworkTimeout(err)
}

// IsServiceUnreached reports a request that did not reach OneDrive, or got
// no answer from it: every request timeout (IsRequestTimeout), and a name
// that does not resolve, a connection refused, a host or network that cannot
// be reached and a connection reset (retry.IsUnreached). It is the one
// family the engine reads as the service's silence: each member is retry work
// and evidence for the service scope while the run goes on, and none is an
// issue that names a file, since nothing is wrong with the file. A failure
// that is plainly local (no route, network unreachable) gets the same scope
// and the same line: the user's remedy is the same, and the trial that ends
// the scope finds the network back either way. A chain that holds a local
// file error is never one; neither is a store access that outlasted its bound.
func IsServiceUnreached(err error) bool {
	if errors.Is(err, ErrStoreAccessTimedOut) {
		return false
	}

	return IsRequestTimeout(err) || retry.IsUnreached(err)
}

func classifyServerStatus(status int) ServerAnswer {
	switch {
	case status == http.StatusUnauthorized:
		return ServerAnswerUnauthorized
	case status == http.StatusForbidden:
		return ServerAnswerForbidden
	case status == http.StatusTooManyRequests:
		return ServerAnswerThrottled
	case status >= http.StatusInternalServerError:
		return ServerAnswerServiceOutage
	case status == http.StatusRequestTimeout:
		return ServerAnswerRequestTimeout
	case status == http.StatusPreconditionFailed:
		return ServerAnswerTransientConflict
	case status == http.StatusNotFound:
		return ServerAnswerTransientNotFound
	case status == http.StatusLocked:
		return ServerAnswerLocked
	default:
		return ServerAnswerOther
	}
}
