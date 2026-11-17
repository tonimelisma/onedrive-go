package sync

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
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

// serverAnswer is what a Graph failure means for the work around it. It is
// the one table from (HTTP status, Graph code, message) to meaning: scope
// detection, result classification and issue naming all read it, so a quota
// answer or a refusal is recognized identically everywhere. The most specific
// evidence wins: a Graph code or a known message is checked before the bare
// status, because OneDrive reports one condition under several statuses. A
// request OneDrive never answered (isRequestTimeout) means what a 5xx means:
// the service is not answering.
type serverAnswer int

const (
	serverAnswerNone serverAnswer = iota
	serverAnswerUnauthorized
	serverAnswerForbidden
	serverAnswerThrottled
	// serverAnswerQuota is a full OneDrive: 507, or quotaLimitReached /
	// insufficientStorage or an "insufficient space" message under any
	// status (Business has returned it as 500 and 403).
	serverAnswerQuota
	serverAnswerServiceOutage
	// serverAnswerUnsupported is OneDrive refusing an operation it does not
	// implement (501, 505, or a notSupported code under any status). It is a
	// stable answer about one request, not an outage, so it must never count
	// toward the service scope: counted, five refusals would block every
	// drive.
	serverAnswerUnsupported
	serverAnswerPathTooLong
	serverAnswerRequestTimeout
	// serverAnswerTransientConflict is 412, and 409 resourceModified: the
	// item changed between our read and our write (an mtime PATCH racing a
	// server-side post-upload update), so a retry against fresh truth works.
	serverAnswerTransientConflict
	serverAnswerTransientNotFound
	serverAnswerLocked
	// serverAnswerTransferURLRejected is a pre-authenticated upload or
	// download URL refusing its own credential (graph.ErrPreAuthURLRejected).
	// Only that file's transfer is affected; it is retried with a new URL.
	serverAnswerTransferURLRejected
	// serverAnswerMalware is content OneDrive flagged as malware
	// (graph.ErrMalwareDetected). It is a permanent per-file issue: probed as
	// a permission problem, the download's 403 would be labeled read-denied
	// and retried forever.
	serverAnswerMalware
	serverAnswerOther
)

// Graph codes and message fragments the server-answer table recognizes
// beyond graph.IsQuotaExhausted and graph.IsNotSupported.
const (
	graphCodeResourceModified = "resourceModified"
	graphMessageNameTooLong   = "name is too long"
	graphMessagePathLimit     = "400 characters or less"
)

// classifyServerAnswer maps one Graph failure to its meaning. status is the
// HTTP status recorded on the completion; err supplies the code and message
// when it carries a graph.GraphError.
func classifyServerAnswer(status int, err error) serverAnswer {
	if errors.Is(err, graph.ErrMalwareDetected) {
		// Checked before the status: the facet check refuses before any
		// request, so there may be no status at all.
		return serverAnswerMalware
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
		return serverAnswerOther
	case errors.Is(err, graph.ErrAuthTransient):
		// A 401 that a just-refreshed token still got, naming a server-side
		// validation fault: an outage, never "sign-in required" (D-072).
		return serverAnswerServiceOutage
	case errors.Is(err, graph.ErrPreAuthURLRejected):
		return serverAnswerTransferURLRejected
	case status == http.StatusInsufficientStorage, graph.IsQuotaExhausted(err):
		return serverAnswerQuota
	case status == http.StatusNotImplemented, status == http.StatusHTTPVersionNotSupported, graph.IsNotSupported(err):
		return serverAnswerUnsupported
	case status == http.StatusBadRequest &&
		(strings.Contains(message, graphMessageNameTooLong) || strings.Contains(message, graphMessagePathLimit)):
		// OneDrive counts more than the drive-relative path toward its 400
		// limit (a 364-character path was accepted and 365 refused), so the
		// scanner cannot catch every case, and a bare 400 would retry forever.
		return serverAnswerPathTooLong
	case status == http.StatusConflict && hasGraphErr && graphErr.HasCode(graphCodeResourceModified):
		return serverAnswerTransientConflict
	}

	return classifyServerStatus(status)
}

// unansweredServerAnswer is the meaning of a failure that carries no status:
// a request that ran out of time waiting for OneDrive is the service's
// silence, the outage a 5xx says out loud; any other carries no answer.
func unansweredServerAnswer(err error) serverAnswer {
	if isRequestTimeout(err) {
		return serverAnswerServiceOutage
	}

	return serverAnswerNone
}

// isRequestTimeout reports a request that ran out of time waiting for
// OneDrive, or for the Microsoft sign-in its token comes from. The dial,
// response-header and whole-request timeouts and an expired request deadline
// carry context.DeadlineExceeded; a transfer the stall watchdog abandoned
// carries graph.ErrTransferStalled instead, because the watchdog keeps its
// cancellation out of the chain; an answer whose body brought nothing for the
// stall window carries graph.ErrResponseStalled, and a request or body read
// on an HTTP/2 connection closed for an unanswered ping
// graph.ErrConnectionLost; a TLS handshake timeout and a read whose
// connection's keepalive found the server gone carry none of these
// (isNetworkTimeout). An outcome commit or created-item record that outlasted
// its bound carries context.DeadlineExceeded too, but it is no request:
// errStoreAccessTimedOut marks it. Whether the run had ended is not asked
// here: classifyResult asks.
func isRequestTimeout(err error) bool {
	if errors.Is(err, errStoreAccessTimedOut) {
		return false
	}

	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, graph.ErrTransferStalled) ||
		errors.Is(err, graph.ErrResponseStalled) || errors.Is(err, graph.ErrConnectionLost) ||
		isNetworkTimeout(err)
}

// isNetworkTimeout reports a failure the HTTP client or a connection says
// timed out: net/http wraps every transport failure in *url.Error, and a read
// on a connection fails as *net.OpError, and both answer Timeout() for what
// they wrap. A chain that holds a local file error is never one: an upload's
// body is read from the local file, and a reader that returned a timed-out
// read of it unwrapped would make the request's *url.Error say it timed out.
func isNetworkTimeout(err error) bool {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return false
	}

	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return true
	}
	var opErr *net.OpError

	return errors.As(err, &opErr) && opErr.Timeout()
}

func classifyServerStatus(status int) serverAnswer {
	switch {
	case status == http.StatusUnauthorized:
		return serverAnswerUnauthorized
	case status == http.StatusForbidden:
		return serverAnswerForbidden
	case status == http.StatusTooManyRequests:
		return serverAnswerThrottled
	case status >= http.StatusInternalServerError:
		return serverAnswerServiceOutage
	case status == http.StatusRequestTimeout:
		return serverAnswerRequestTimeout
	case status == http.StatusPreconditionFailed:
		return serverAnswerTransientConflict
	case status == http.StatusNotFound:
		return serverAnswerTransientNotFound
	case status == http.StatusLocked:
		return serverAnswerLocked
	default:
		return serverAnswerOther
	}
}
