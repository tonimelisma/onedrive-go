package retry_test

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// unreachedGraphURL is where the requests below were sent.
const unreachedGraphURL = "https://graph.example/v1.0/me/drive/items/root/children"

// Each way a request does not reach the service, built as net/http returns
// it: a *url.Error around a *net.OpError around the errno's *os.SyscallError,
// or around the resolver's *net.DNSError. A failure that only reads like one
// (the errno with no network operation, a local file read) is not.
//
// Validates: R-2.10.3
func TestIsUnreached_Table(t *testing.T) {
	t.Parallel()

	dial := func(errno syscall.Errno) error {
		return &url.Error{Op: http.MethodPost, URL: unreachedGraphURL, Err: &net.OpError{
			Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errno),
		}}
	}
	read := func(errno syscall.Errno) error {
		return &url.Error{Op: http.MethodGet, URL: unreachedGraphURL, Err: &net.OpError{
			Op: "read", Net: "tcp", Err: os.NewSyscallError("read", errno),
		}}
	}
	notFound := &url.Error{Op: http.MethodGet, URL: unreachedGraphURL, Err: &net.OpError{
		Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "graph.example", IsNotFound: true},
	}}

	testCases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "connection refused", err: dial(syscall.ECONNREFUSED), want: true},
		{name: "no route to host", err: dial(syscall.EHOSTUNREACH), want: true},
		{name: "network unreachable", err: dial(syscall.ENETUNREACH), want: true},
		{name: "connection reset", err: read(syscall.ECONNRESET), want: true},
		{name: "name not found", err: fmt.Errorf("list children: %w", notFound), want: true},
		{name: "timed-out read", err: read(syscall.ETIMEDOUT), want: true},
		{name: "nil", err: nil, want: false},
		{name: "the errno without a network operation", err: fmt.Errorf("x: %w", syscall.ECONNREFUSED), want: false},
		{name: "a local file read that timed out", err: &url.Error{
			Op: http.MethodPut, URL: unreachedGraphURL,
			Err: &fs.PathError{Op: "read", Path: "/sync/a.bin", Err: syscall.ETIMEDOUT},
		}, want: false},
		{name: "a plain error", err: errors.New("boom"), want: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, retry.IsUnreached(tc.err), "%v", tc.err)
		})
	}
}
