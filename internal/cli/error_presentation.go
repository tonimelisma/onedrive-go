package cli

import (
	"errors"
	"net"
	"strings"
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

	return ""
}

// commandErrorText is what main prints for a failed command: one plain line
// for the failures a user can fix, the error itself otherwise. details adds
// the full chain for --verbose/--debug.
func commandErrorText(err error, details bool) string {
	if authMessage := authErrorMessage(err); authMessage != "" {
		return authMessage
	}

	if reason := networkErrorReason(err); reason != "" {
		text := "Error: " + reason + ". Check your network connection, then run the command again."
		if details {
			text += "\nDetails: " + err.Error()
		}
		return text
	}

	return "Error: " + err.Error()
}
