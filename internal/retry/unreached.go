package retry

import (
	"errors"
	"io/fs"
	"net"
	"net/url"
	"syscall"
)

// IsUnreached reports a request that did not reach the service, or reached
// it and got no answer, as the HTTP client and the network stack say it: a
// name that does not resolve (*net.DNSError), a connection refused, a host or
// network that cannot be reached, a connection reset, and a dial, handshake,
// response-header or read that timed out. Each says nothing about the request
// or the item it names: the same request can succeed once the network or the
// service is back. RetryTransport retries every transport failure of a
// replayable request, these and the rest (a TLS failure, a connection that
// dropped mid-exchange), as one family it announces as "can't reach
// OneDrive"; this predicate is the part of that family a caller that does not
// retry in the transport (the sync engine) reads as the service's silence.
//
// A chain that holds a local file error is never one: an upload's body is read
// from the local file, and a timed-out or reset read of it, returned unwrapped
// by its reader, would make the request's *url.Error look like the network's.
func IsUnreached(err error) bool {
	if err == nil {
		return false
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return false
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}

	return IsNetworkTimeout(err) || isConnectionFailure(err)
}

// IsNetworkTimeout reports a failure the HTTP client or a connection says
// timed out: net/http wraps every transport failure in *url.Error, and a read
// on a connection fails as *net.OpError, and both answer Timeout() for what
// they wrap. A chain that holds a local file error is never one (IsUnreached).
func IsNetworkTimeout(err error) bool {
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

// isConnectionFailure reports a connection the network refused, could not
// route, or reset, by the errno a *net.OpError holds. The errno alone is not
// enough: only a network operation's failure is the network's.
func isConnectionFailure(err error) bool {
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		return false
	}

	return errors.Is(opErr, syscall.ECONNREFUSED) || errors.Is(opErr, syscall.EHOSTUNREACH) ||
		errors.Is(opErr, syscall.ENETUNREACH) || errors.Is(opErr, syscall.ECONNRESET)
}
