package cli

import (
	"bytes"
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

const (
	// stalledSignInDeadline is the whole-request deadline of a sign-in client
	// whose requests never get an answer, short so the test stays fast.
	stalledSignInDeadline = 50 * time.Millisecond
	// failingSignInDeadline is the deadline of a sign-in client whose
	// requests fail at once; no test reaches it.
	failingSignInDeadline = time.Minute

	signInFailureNextStep = ". Check your network connection, then run the command again."
)

// failingSignIn is the transport of a sign-in client whose every request
// fails with err before an answer.
type failingSignIn struct {
	err error
}

func (f failingSignIn) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, f.err
}

// stalledSignIn holds every sign-in request until its client gives up.
type stalledSignIn struct{}

func (stalledSignIn) RoundTrip(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()

	return nil, fmt.Errorf("stalled sign-in request: %w", context.Cause(req.Context()))
}

// refusingSignIn answers every refresh as Microsoft does for a refresh token
// it expired or revoked.
type refusingSignIn struct{}

func (refusingSignIn) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"error":"invalid_grant","error_description":"AADSTS70008: The refresh token has expired."}`)),
		Request: req,
	}, nil
}

// saveExpiredSignIn saves a sign-in for cid whose access token has expired,
// so the first Graph request must refresh it.
func saveExpiredSignIn(t *testing.T, cid driveid.CanonicalID) {
	t.Helper()

	require.NoError(t, tokenfile.Save(config.DriveTokenPath(cid), &oauth2.Token{
		AccessToken:  "expired-" + cid.Email(),
		RefreshToken: "refresh-" + cid.Email(),
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(-time.Hour),
	}))
}

// lsWithSignIn runs `ls /` for a drive whose saved sign-in has expired, with
// the sign-in client signInHTTP, and returns the command's error. OneDrive is
// never asked: the request waits on the refresh.
func lsWithSignIn(t *testing.T, signInHTTP *http.Client) error {
	t.Helper()

	var graphRequests atomic.Int32
	var stdout, stderr bytes.Buffer
	cid := driveid.MustCanonicalID("personal:user@example.com")
	cc := newFileCommandTestContext(t, cid, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		graphRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}), &stdout, &stderr)
	saveExpiredSignIn(t, cid)
	cc.Runtime.TokenSourceFn = func(tokenPath string, logger *slog.Logger) (graph.TokenSource, error) {
		return graph.TokenSourceFromPath(tokenPath, signInHTTP, logger)
	}

	cmd := newLsCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetContext(context.WithValue(t.Context(), cliContextKey{}, cc))

	err := cmd.Execute()
	require.Error(t, err)
	assert.Zero(t, graphRequests.Load(), "a request whose token never came is never sent")

	return fmt.Errorf("ls: %w", err)
}

// Validates: R-6.6.22
//
// A token refresh that fails before the sign-in service answers is named as
// the sign-in service's failure, not OneDrive's, in the same words a Graph
// failure gets.
func TestCommandErrorText_ASignInFailureNamesMicrosoftSignIn(t *testing.T) {
	tests := []struct {
		name    string
		signIn  *http.Client
		wantErr string
	}{
		{
			name:    "a refresh that timed out",
			signIn:  &http.Client{Timeout: stalledSignInDeadline, Transport: stalledSignIn{}},
			wantErr: "Error: Microsoft sign-in did not answer in time" + signInFailureNextStep,
		},
		{
			name:    "a secure connection that failed",
			signIn:  &http.Client{Timeout: failingSignInDeadline, Transport: failingSignIn{err: x509.UnknownAuthorityError{}}},
			wantErr: "Error: the secure connection to Microsoft sign-in failed (x509: certificate signed by unknown authority)" + signInFailureNextStep,
		},
		{
			name:    "a connection that dropped",
			signIn:  &http.Client{Timeout: failingSignInDeadline, Transport: failingSignIn{err: io.ErrUnexpectedEOF}},
			wantErr: "Error: the connection to Microsoft sign-in was interrupted" + signInFailureNextStep,
		},
		{
			name: "a host that could not be looked up",
			signIn: &http.Client{Timeout: failingSignInDeadline, Transport: failingSignIn{
				err: &net.DNSError{Err: "no such host", Name: "login.microsoftonline.com", IsNotFound: true},
			}},
			wantErr: "Error: can't reach Microsoft sign-in (could not look up login.microsoftonline.com)" + signInFailureNextStep,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := lsWithSignIn(t, tt.signIn)
			assert.Equal(t, tt.wantErr, commandErrorText(err, false))
			assert.Equal(t, exitFailure, commandExitCode(err, false))
		})
	}
}

// Validates: R-6.6.22, R-6.6.23
//
// An answer the sign-in service gave is read as it always was: an expired
// sign-in says to sign in again and exits with the sign-in-required status.
func TestCommandErrorText_AnExpiredSignInStillSaysSignInAgain(t *testing.T) {
	err := lsWithSignIn(t, &http.Client{Timeout: failingSignInDeadline, Transport: refusingSignIn{}})

	assert.Equal(t, "Authentication required: Microsoft no longer accepts the saved login for this account "+
		"(it expired or was revoked). Run 'onedrive-go login' to sign in again.", commandErrorText(err, false))
	assert.Equal(t, exitSignInRequired, commandExitCode(err, false))
}

// Validates: R-6.6.22
//
// A Graph request's own timeout still names OneDrive.
func TestCommandErrorText_AGraphTimeoutStillNamesOneDrive(t *testing.T) {
	graphErr := &url.Error{Op: "Get", URL: "https://graph.microsoft.com/v1.0/me", Err: context.DeadlineExceeded}

	assert.Equal(t, "Error: OneDrive did not answer in time"+signInFailureNextStep, commandErrorText(graphErr, false))
}
