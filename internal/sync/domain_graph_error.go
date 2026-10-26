package sync

import (
	"errors"
	"net/http"
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
// status, because OneDrive reports one condition under several statuses.
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
	// toward the service scope: five refusals used to block every drive.
	serverAnswerUnsupported
	serverAnswerPathTooLong
	// serverAnswerTimestampOutOfRange is OneDrive rejecting a fileSystemInfo
	// timestamp (graph.ErrTimestampOutOfRange), reported on the wire as 404.
	serverAnswerTimestampOutOfRange
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
	// (graph.ErrMalwareDetected). It is a permanent per-file issue: before,
	// the download 403 was probed as a permission problem, labeled
	// read-denied and retried forever.
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
		return serverAnswerNone
	}

	var graphErr *graph.GraphError
	hasGraphErr := errors.As(err, &graphErr)
	message := ""
	if hasGraphErr {
		message = strings.ToLower(graphErr.Message)
	}

	switch {
	case errors.Is(err, graph.ErrTimestampOutOfRange):
		return serverAnswerTimestampOutOfRange
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
		// scanner cannot catch every case and the bare 400 retried forever.
		return serverAnswerPathTooLong
	case status == http.StatusConflict && hasGraphErr && graphErr.HasCode(graphCodeResourceModified):
		return serverAnswerTransientConflict
	}

	return classifyServerStatus(status)
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
