package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
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
// `<action> "<path>": <cause>` text for --verbose details and logs. noParent
// marks a path with no parent folder a list could show, such as a shared
// file's selector.
type remoteOpError struct {
	action   string
	path     string
	err      error
	noParent bool
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
	service := failedService(err)

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "can't reach " + service + " (could not look up " + dnsErr.Name + ")"
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) {
		cause := innermostErrorText(opErr)
		if opErr.Op == "proxyconnect" || strings.Contains(err.Error(), "proxyconnect") {
			return "can't reach " + service + " through the configured proxy (" + cause + ")"
		}
		return "can't reach " + service + " (" + cause + ")"
	}

	return transportErrorReason(err, service)
}

// failedService names the service whose request failed: Microsoft sign-in
// when the graph client marked the failure as a request to the sign-in
// service (a token refresh, or a login flow's request), OneDrive otherwise.
func failedService(err error) string {
	var signIn *graph.SignInRequestError
	if errors.As(err, &signIn) {
		return "Microsoft sign-in"
	}

	return "OneDrive"
}

// transportErrorReason covers network failures after the connection was
// made: timeouts (including a response-header timeout), TLS problems, and
// connections that dropped or were reset, HTTP/2 stream resets included. It
// only applies to errors that came from the HTTP transport: a *url.Error
// (before the response) or a *graph.ResponseReadError (while reading it). So
// a local file timeout, or a disk error io.Copy returned during a download,
// is never described as a network problem. service names the one whose
// request failed (failedService).
func transportErrorReason(err error, service string) string {
	var urlErr *url.Error
	var readErr *graph.ResponseReadError
	fromURL := errors.As(err, &urlErr)
	if !fromURL && !errors.As(err, &readErr) {
		return ""
	}

	switch {
	case fromURL && urlErrorTimedOut(urlErr), !fromURL && isTimeout(readErr.Err):
		return service + " did not answer in time"
	case isTLSFailure(err):
		return "the secure connection to " + service + " failed (" + innermostErrorText(err) + ")"
	case isDroppedConnection(err):
		return "the connection to " + service + " was interrupted"
	default:
		return ""
	}
}

// urlErrorTimedOut reports whether a request failed on a timeout. A
// *url.Error's own Timeout asks only the error it holds directly, so a round
// tripper that wrapped the deadline it hit would hide it, and the failure
// would be reported without naming the service. The chain below is read too.
func urlErrorTimedOut(urlErr *url.Error) bool {
	return urlErr.Timeout() || isTimeout(urlErr.Err)
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func isTLSFailure(err error) bool {
	var certErr *tls.CertificateVerificationError
	var recordErr tls.RecordHeaderError
	var alertErr tls.AlertError
	var authorityErr x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var invalidErr x509.CertificateInvalidError

	return errors.As(err, &certErr) || errors.As(err, &recordErr) || errors.As(err, &alertErr) ||
		errors.As(err, &authorityErr) || errors.As(err, &hostnameErr) || errors.As(err, &invalidErr)
}

// isDroppedConnection recognizes a connection that ended mid-exchange. net/http
// does not export its HTTP/2 stream and connection errors, so those are
// recognized by their fixed "stream error:" and "http2:" text.
func isDroppedConnection(err error) bool {
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) {
		return true
	}

	text := err.Error()

	return strings.Contains(text, "stream error:") || strings.Contains(text, "http2: ")
}

// remoteFailureSubject returns the path a command recorded for its failure
// (or "the item") and, when the path has a parent, a hint to list it.
func remoteFailureSubject(err error) (*remoteOpError, string, string) {
	var op *remoteOpError
	if !errors.As(err, &op) {
		return nil, "the item", ""
	}

	hint := ""
	if parent := path.Dir(op.path); parent != op.path && !op.noParent {
		hint = " Run 'onedrive-go ls " + parent + "' to see what is there."
	}

	return op, op.path, hint
}

func notImplementedMessage(err error) string {
	var graphErr *graph.GraphError
	if errors.As(err, &graphErr) && graphErr.Message != "" {
		return graphErr.Message
	}

	return "the request is not supported"
}

// remoteFailureText turns the OneDrive failures a user can understand into
// one sentence with a next step, naming the path when the command recorded
// one. It returns "" for failures it does not recognize.
func remoteFailureText(err error) string {
	op, subject, parentHint := remoteFailureSubject(err)
	movesOrCopies := op != nil && (strings.HasPrefix(op.action, "moving") || strings.HasPrefix(op.action, "copying"))

	switch {
	case graph.IsQuotaExhausted(err):
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
	case graph.IsNotSupported(err):
		// 501 is OneDrive refusing the request, not an outage: retrying
		// "in a few minutes" cannot help, so pass on what it said.
		return "OneDrive cannot do this for " + subject + ": " + notImplementedMessage(err)
	case errors.Is(err, graph.ErrServerError):
		return "OneDrive had a temporary server problem. Try again in a few minutes."
	default:
		return ""
	}
}

// localFailureText recognizes local filesystem failures a user can fix: a
// full disk (including the sync state database failing with SQLITE_FULL), a
// read-only filesystem, a file the filesystem cannot hold, and missing or
// unreadable files. The errno meaning comes from driveops' shared table. A
// download's hidden partial file is named by its folder, the one the user
// knows, where the folder is at fault: a read-only filesystem, a file too
// large for it, or a permission denied while the partial does not exist. A
// partial that exists and denies permission is itself at fault and is named.
// A partial that is missing has no plain sentence, since its folder is
// there, and the error itself is printed.
func localFailureText(err error) string {
	if syncstore.IsLocalDiskFull(err) {
		return "The local disk is full (or below the min_free_space reserve). Free up space, then run the command again."
	}

	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) {
		return ""
	}
	subject := pathErr.Path
	if driveops.IsOwnedTransferArtifactName(filepath.Base(subject)) {
		if errors.Is(err, fs.ErrNotExist) {
			return ""
		}
		if _, statErr := localpath.Lstat(subject); statErr != nil || !errors.Is(err, fs.ErrPermission) {
			subject = filepath.Dir(subject)
		}
	}

	switch driveops.ClassifyLocalError(err) {
	case driveops.LocalErrorReadOnly:
		return subject + " is on a read-only filesystem. Remount it read-write, then run the command again."
	case driveops.LocalErrorFileTooLarge:
		return "The file is too large for the filesystem holding " + subject + "."
	case driveops.LocalErrorOther,
		driveops.LocalErrorNoSpace,
		driveops.LocalErrorBusy,
		driveops.LocalErrorCrossDevice,
		driveops.LocalErrorNameTooLong:
	}

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return subject + " does not exist."
	case errors.Is(err, fs.ErrPermission):
		return "Permission denied for " + subject + ". Check its file permissions."
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
