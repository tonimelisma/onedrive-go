package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"syscall"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// presentedError carries a user-facing message while keeping the underlying
// error reachable through errors.Is/As, so classification (auth, network,
// shutdown) still works after a message has been composed for display.
type presentedError struct {
	message string
	err     error
}

func (e *presentedError) Error() string { return e.message }

func (e *presentedError) Unwrap() error { return e.err }

func newPresentedError(message string, err error) error {
	if message == "" {
		return err
	}

	return &presentedError{message: message, err: err}
}

// remoteOpError names the OneDrive path a command was working on when it
// failed, so main can say "/docs/a.txt was not found on OneDrive" instead of
// printing the whole wrapped chain. Error() keeps the familiar
// `<action> "<path>": <cause>` text for --verbose details and logs.
type remoteOpError struct {
	action string
	path   string
	err    error
}

func (e *remoteOpError) Error() string {
	return e.action + " " + strconv.Quote(e.path) + ": " + e.err.Error()
}

func (e *remoteOpError) Unwrap() error { return e.err }

func newRemoteOpError(action, remotePath string, err error) error {
	return &remoteOpError{action: action, path: remotePath, err: err}
}

// usageError marks a command-line mistake (unknown command or flag, wrong
// argument count, conflicting flags). main prints it with a pointer to --help
// and exits exitUsage (2), the conventional usage-error status.
type usageError struct {
	commandPath string
	err         error
}

func (e *usageError) Error() string { return e.err.Error() }

func (e *usageError) Unwrap() error { return e.err }

// networkErrorReason recognizes "could not reach the server" failures and
// names the concrete cause, or returns "" for anything else.
func networkErrorReason(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "can't reach OneDrive (could not look up " + dnsErr.Name + ")"
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) {
		cause := innermostErrorText(opErr)
		if opErr.Op == "proxyconnect" || strings.Contains(err.Error(), "proxyconnect") {
			return "can't reach OneDrive through the configured proxy (" + cause + ")"
		}
		return "can't reach OneDrive (" + cause + ")"
	}

	return transportErrorReason(err)
}

// transportErrorReason covers network failures after the connection was
// made: timeouts, TLS problems and dropped connections. It only applies to
// errors that came from the HTTP transport (*url.Error), so a local file
// timeout is never described as a OneDrive problem.
func transportErrorReason(err error) string {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return ""
	}

	var certErr *tls.CertificateVerificationError
	var recordErr tls.RecordHeaderError
	switch {
	case urlErr.Timeout():
		return "OneDrive did not answer in time"
	case errors.As(err, &certErr), errors.As(err, &recordErr):
		return "the secure connection to OneDrive failed (" + innermostErrorText(err) + ")"
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF), errors.Is(err, syscall.ECONNRESET):
		return "the connection to OneDrive was interrupted"
	default:
		return ""
	}
}

// remoteFailureSubject returns the path a command recorded for its failure
// (or "the item") and, when the path has a parent, a hint to list it.
func remoteFailureSubject(err error) (*remoteOpError, string, string) {
	var op *remoteOpError
	if !errors.As(err, &op) {
		return nil, "the item", ""
	}

	hint := ""
	if parent := path.Dir(op.path); parent != op.path {
		hint = " Run 'onedrive-go ls " + parent + "' to see what is there."
	}

	return op, op.path, hint
}

func isQuotaFull(err error) bool {
	var graphErr *graph.GraphError
	return errors.As(err, &graphErr) &&
		(graphErr.StatusCode == http.StatusInsufficientStorage || graphErr.HasCode("quotaLimitReached"))
}

// remoteFailureText turns the OneDrive failures a user can understand into
// one sentence with a next step, naming the path when the command recorded
// one. It returns "" for failures it does not recognize.
func remoteFailureText(err error) string {
	op, subject, parentHint := remoteFailureSubject(err)
	movesOrCopies := op != nil && (strings.HasPrefix(op.action, "moving") || strings.HasPrefix(op.action, "copying"))

	switch {
	case isQuotaFull(err):
		return "Your OneDrive storage is full. Free up space or add storage, then run the command again."
	case errors.Is(err, graph.ErrNotFound):
		return subject + " was not found on OneDrive." + parentHint
	case errors.Is(err, graph.ErrForbidden):
		return "OneDrive denied access to " + subject + ". Check that this account can open it."
	case errors.Is(err, graph.ErrConflict) && movesOrCopies:
		return "The destination for " + subject + " already exists on OneDrive. Pass --force to replace it."
	case errors.Is(err, graph.ErrConflict):
		return subject + " already exists on OneDrive or was changed at the same time. Check it and run the command again."
	case errors.Is(err, graph.ErrLocked):
		return subject + " is locked on OneDrive, usually because it is open in another app. Try again later."
	case errors.Is(err, graph.ErrThrottled):
		return "OneDrive is limiting requests right now. Wait a few minutes, then run the command again."
	case errors.Is(err, graph.ErrProviderReadOnly):
		return "OneDrive is temporarily read-only for this account. Try again later."
	case errors.Is(err, graph.ErrServerError):
		return "OneDrive had a temporary server problem. Try again in a few minutes."
	default:
		return ""
	}
}

// localFailureText recognizes missing and unreadable local files.
func localFailureText(err error) string {
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) {
		return ""
	}

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return pathErr.Path + " does not exist."
	case errors.Is(err, fs.ErrPermission):
		return "Permission denied for " + pathErr.Path + ". Check its file permissions."
	default:
		return ""
	}
}

// commandErrorText is what main prints for a failed command: one plain line
// for the failures a user can understand or fix, the error itself otherwise.
// details adds the full chain for --verbose/--debug.
func commandErrorText(err error, details bool) string {
	if authMessage := authErrorMessage(err); authMessage != "" {
		return authMessage
	}

	var usage *usageError
	if errors.As(err, &usage) {
		return "Error: " + usage.err.Error() + "\nRun '" + usage.commandPath + " --help' for usage."
	}

	plain := networkErrorReason(err)
	if plain != "" {
		plain += ". Check your network connection, then run the command again."
	}
	if plain == "" && errors.Is(err, context.DeadlineExceeded) {
		plain = "The operation timed out. Run the command again."
	}
	if plain == "" {
		plain = remoteFailureText(err)
	}
	if plain == "" {
		plain = localFailureText(err)
	}

	if plain == "" {
		return "Error: " + err.Error()
	}

	text := "Error: " + plain
	if details {
		text += "\nDetails: " + err.Error()
	}

	return text
}
