package syncexec

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/retry"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// This file is where a Graph failure becomes the facts a completion records:
// its HTTP status, the server's Retry-After and the server's answer
// (syncdomain.ServerAnswer). The executor is the package that talks to Graph,
// so it is the one that reads Graph's errors; the vocabulary records what
// they meant and imports no transport.

// CompletionFromTrackedAction builds the completion of one exact tracked
// action: the one constructor of every production completion. Workers and the
// engine's admission and publication-drain stages all build theirs here, so
// classification and persistence see one consistent action identity, and the
// server's answer is read once, from the error the completion carries. No
// one changes a completion's error after it is built.
func CompletionFromTrackedAction(
	ta *syncdomain.TrackedAction,
	outcome *syncdomain.ActionOutcome,
	actionErr error,
) syncdomain.ActionCompletion {
	driveID := ta.Action.DriveID
	if outcome != nil && !outcome.DriveID.IsZero() {
		driveID = outcome.DriveID
	}

	r := syncdomain.ActionCompletion{
		Path:                 ta.Action.Path,
		OldPath:              ta.Action.OldPath,
		ItemID:               ta.Action.ItemID,
		DriveID:              driveID,
		ActionType:           ta.Action.Type,
		ItemType:             syncdomain.ResolveItemType(ta.Action.View),
		Err:                  actionErr,
		IsTrial:              ta.IsTrial,
		TrialScopeKey:        ta.TrialScopeKey,
		TrialScopeGeneration: ta.TrialScopeGeneration,
		ActionID:             ta.ID,
	}

	if outcome != nil {
		r.Success = outcome.Success
		r.FailurePath = outcome.FailurePath
		r.FailureCapability = outcome.FailureCapability
		r.Size, r.SizeKnown = outcomeDisplaySize(outcome)
		if outcome.Error != nil {
			r.Err = outcome.Error
			r.ErrMsg = outcome.Error.Error()
		}
	} else if actionErr != nil {
		r.ErrMsg = actionErr.Error()
	}

	recordServerAnswer(&r, ExtractHTTPStatus(r.Err))

	return r
}

// recordServerAnswer sets the facts a completion keeps of its Graph failure:
// the status it was answered with, the server's Retry-After and what the two
// mean together.
func recordServerAnswer(r *syncdomain.ActionCompletion, status int) {
	r.HTTPStatus = status
	r.RetryAfter = extractRetryAfter(r.Err)
	r.ServerAnswer = classifyServerAnswer(status, r.Err)
}

// outcomeDisplaySize picks the size a user would associate with the action:
// what arrived for a download, what was sent for an upload.
func outcomeDisplaySize(outcome *syncdomain.ActionOutcome) (int64, bool) {
	switch outcome.Action {
	case syncdomain.ActionDownload:
		return outcome.RemoteSize, outcome.RemoteSizeKnown
	case syncdomain.ActionUpload:
		return outcome.LocalSize, outcome.LocalSizeKnown
	case syncdomain.ActionLocalDelete, syncdomain.ActionRemoteDelete, syncdomain.ActionLocalMove,
		syncdomain.ActionRemoteMove, syncdomain.ActionFolderCreate, syncdomain.ActionConflictCopy,
		syncdomain.ActionBaselineUpdate, syncdomain.ActionCleanup:
		return 0, false
	default:
		panic(syncdomain.OutsideTheEnum(outcome.Action))
	}
}

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

// Graph codes and message fragments the server-answer table recognizes
// beyond graph.IsQuotaExhausted and graph.IsNotSupported.
const (
	GraphCodeResourceModified = "resourceModified"
	graphMessageNameTooLong   = "name is too long"
	graphMessagePathLimit     = "400 characters or less"
)

// classifyServerAnswer is the one table from (HTTP status, Graph code,
// message) to what a Graph failure means (syncdomain.ServerAnswer): scope
// detection, result classification and issue naming all read the answer it
// records on a completion, so a quota answer or a refusal is recognized
// identically everywhere. The most specific evidence wins: a Graph code or a
// known message is checked before the bare status, because OneDrive reports
// one condition under several statuses. A request OneDrive never answered
// (IsRequestTimeout) means what a 5xx means: the service is not answering.
// status is the HTTP status recorded on the completion; err supplies the
// code and message when it carries a graph.GraphError.
func classifyServerAnswer(status int, err error) syncdomain.ServerAnswer {
	if errors.Is(err, graph.ErrMalwareDetected) {
		// Checked before the status: the facet check refuses before any
		// request, so there may be no status at all.
		return syncdomain.ServerAnswerMalware
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
		return syncdomain.ServerAnswerOther
	case errors.Is(err, graph.ErrAuthTransient):
		// A 401 that a just-refreshed token still got, naming a server-side
		// validation fault: an outage, never "sign-in required" (D-072).
		return syncdomain.ServerAnswerServiceOutage
	case errors.Is(err, graph.ErrPreAuthURLRejected):
		return syncdomain.ServerAnswerTransferURLRejected
	case status == http.StatusInsufficientStorage, graph.IsQuotaExhausted(err):
		return syncdomain.ServerAnswerQuota
	case status == http.StatusNotImplemented, status == http.StatusHTTPVersionNotSupported, graph.IsNotSupported(err):
		return syncdomain.ServerAnswerUnsupported
	case status == http.StatusBadRequest &&
		(strings.Contains(message, graphMessageNameTooLong) || strings.Contains(message, graphMessagePathLimit)):
		// OneDrive accepted a path below the drive root of 364 UTF-16 units
		// and refused one of 365 (L-36; why below its stated 400 is not
		// known), so the scanner's 400 cannot catch every case, and a bare
		// 400 would retry forever.
		return syncdomain.ServerAnswerPathTooLong
	case status == http.StatusConflict && hasGraphErr && graphErr.HasCode(GraphCodeResourceModified):
		return syncdomain.ServerAnswerTransientConflict
	}

	return classifyServerStatus(status)
}

// unansweredServerAnswer is the meaning of a failure that carries no status:
// a request that did not reach OneDrive or got no answer is the service's
// silence, the outage a 5xx says out loud; any other carries no answer.
func unansweredServerAnswer(err error) syncdomain.ServerAnswer {
	if IsServiceUnreached(err) {
		return syncdomain.ServerAnswerServiceOutage
	}

	return syncdomain.ServerAnswerNone
}

func classifyServerStatus(status int) syncdomain.ServerAnswer {
	switch {
	case status == http.StatusUnauthorized:
		return syncdomain.ServerAnswerUnauthorized
	case status == http.StatusForbidden:
		return syncdomain.ServerAnswerForbidden
	case status == http.StatusTooManyRequests:
		return syncdomain.ServerAnswerThrottled
	case status >= http.StatusInternalServerError:
		return syncdomain.ServerAnswerServiceOutage
	case status == http.StatusRequestTimeout:
		return syncdomain.ServerAnswerRequestTimeout
	case status == http.StatusPreconditionFailed:
		return syncdomain.ServerAnswerTransientConflict
	case status == http.StatusNotFound:
		return syncdomain.ServerAnswerTransientNotFound
	case status == http.StatusLocked:
		return syncdomain.ServerAnswerLocked
	default:
		return syncdomain.ServerAnswerOther
	}
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
// (retry.IsNetworkTimeout). An outcome commit or created-item record that
// outlasted its bound carries context.DeadlineExceeded too, but it is no
// request: syncdomain.ErrStoreAccessTimedOut marks it. A transfer the
// watchdog abandoned because its local side stopped moving
// (graph.ErrLocalSideStalled: an upload's read or a download's write of the
// local file hung) waited on the local disk, not on OneDrive: it is no
// request timeout, and the local classifier retries the one file as busy.
// Whether the run had ended is not asked here: the engine's classifier asks.
func IsRequestTimeout(err error) bool {
	if errors.Is(err, syncdomain.ErrStoreAccessTimedOut) || errors.Is(err, graph.ErrLocalSideStalled) {
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
	if errors.Is(err, syncdomain.ErrStoreAccessTimedOut) {
		return false
	}

	return IsRequestTimeout(err) || retry.IsUnreached(err)
}
