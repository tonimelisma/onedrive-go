package graph

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

// Where the identity platform serves the "common" tenant. The sign-in flows
// address these; the tests' auth client delivers them to a local server.
// signInGrantPath is the token endpoint, which grants every token.
const (
	signInHost           = "login.microsoftonline.com"
	signInGrantPath      = "/common/oauth2/v2.0/token"
	signInDeviceCodePath = "/common/oauth2/v2.0/devicecode"

	// stalledSignInTimeout is the injected client's whole-request deadline in
	// the stalled-endpoint test, short so the test stays fast.
	stalledSignInTimeout = 50 * time.Millisecond

	// signInFormLimit bounds the refresh request form the stand-in reads.
	signInFormLimit = 4 << 10

	savedRefreshToken = "saved-refresh-token"
)

// signInStandIn is the RoundTripper of a test's auth client. It sends every
// request to a local httptest server in place of the identity platform,
// keeping the path, and records where the flow addressed each request, so a
// test can prove which HTTP client a sign-in flow used.
type signInStandIn struct {
	target *url.URL

	mu        sync.Mutex // guards addressed
	addressed []string   // host and path of each request, as the flow built it
}

func newSignInStandIn(t *testing.T, srv *httptest.Server) *signInStandIn {
	t.Helper()

	target, err := url.Parse(srv.URL)
	require.NoError(t, err)

	return &signInStandIn{target: target}
}

func (s *signInStandIn) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.addressed = append(s.addressed, req.URL.Host+req.URL.Path)
	s.mu.Unlock()

	local := req.Clone(req.Context())
	local.URL.Scheme = s.target.Scheme
	local.URL.Host = s.target.Host
	local.Host = s.target.Host

	resp, err := http.DefaultTransport.RoundTrip(local)
	if err != nil {
		return nil, fmt.Errorf("deliver sign-in request to the local stand-in: %w", err)
	}

	return resp, nil
}

func (s *signInStandIn) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.addressed)
}

// offlineSignIn is the RoundTripper of tests whose saved token never needs a
// refresh: any request it is asked to send fails the test.
type offlineSignIn struct {
	t *testing.T
}

func (o offlineSignIn) RoundTrip(req *http.Request) (*http.Response, error) {
	assert.Failf(o.t, "unexpected sign-in request", "%s %s%s", req.Method, req.URL.Host, req.URL.Path)

	return nil, fmt.Errorf("unexpected sign-in request to %s", req.URL.Host)
}

func offlineAuthClient(t *testing.T) *http.Client {
	t.Helper()

	return &http.Client{Transport: offlineSignIn{t: t}}
}

// writeExpiredSignIn saves a sign-in whose access token has expired, so the
// first Token call must refresh it.
func writeExpiredSignIn(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "tokens", "expired.json")
	require.NoError(t, tokenfile.Save(path, &oauth2.Token{
		AccessToken:  "expired-access-token",
		TokenType:    "Bearer",
		RefreshToken: savedRefreshToken,
		Expiry:       time.Now().Add(-time.Hour),
	}))

	return path
}

func writeSignInJSON(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	writeTestResponse(t, w, body)
}

// Validates: R-6.8.27
func TestTokenSourceFromPath_RefreshUsesTheInjectedClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, signInGrantPath, r.URL.Path)
		r.Body = http.MaxBytesReader(w, r.Body, signInFormLimit)
		assert.NoError(t, r.ParseForm())
		assert.Equal(t, "refresh_token", r.PostForm.Get("grant_type"))
		assert.Equal(t, savedRefreshToken, r.PostForm.Get("refresh_token"))
		writeSignInJSON(t, w, `{
			"access_token": "refreshed-access-token",
			"token_type": "Bearer",
			"refresh_token": "rotated-refresh-token",
			"expires_in": 3600
		}`)
	}))
	t.Cleanup(srv.Close)

	standIn := newSignInStandIn(t, srv)
	path := writeExpiredSignIn(t)

	ts, err := TokenSourceFromPath(t.Context(), path, &http.Client{Transport: standIn}, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	got, err := ts.Token()
	require.NoError(t, err)
	assert.Equal(t, "refreshed-access-token", got)
	assert.Equal(t, []string{signInHost + signInGrantPath}, standIn.requests(),
		"the silent refresh must go through the client the caller injected")

	saved, err := tokenfile.Load(path)
	require.NoError(t, err)
	assert.Equal(t, "rotated-refresh-token", saved.RefreshToken)
}

// Validates: R-6.8.27
//
// A token endpoint that accepts the connection and never answers used to hold
// the refresh, and every request waiting behind the token source's lock, until
// the process stopped. With a deadline on the auth client both callers get an
// error instead.
func TestTokenSourceFromPath_StalledTokenEndpointTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	// Cleanups run last-in first-out: the handlers are released before
	// Close waits for them.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	authHTTP := &http.Client{Timeout: stalledSignInTimeout, Transport: newSignInStandIn(t, srv)}
	ts, err := TokenSourceFromPath(t.Context(), writeExpiredSignIn(t), authHTTP, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	const callers = 2
	errs := make([]error, callers)

	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			_, errs[i] = ts.Token()
		})
	}
	wg.Wait()

	// The client's Timeout is the only deadline in play (t.Context has none),
	// so DeadlineExceeded in the chain says the auth client released the
	// caller. net/http reports it either as its own timeout error or as the
	// transport's context error, depending on which notices first.
	for i, tokenErr := range errs {
		require.Error(t, tokenErr, "caller %d must be released with an error", i)
		require.ErrorIs(t, tokenErr, context.DeadlineExceeded,
			"caller %d must fail on the auth client's deadline: %v", i, tokenErr)
	}
}

// Validates: R-6.8.27
func TestTokenSourceFromPath_NilAuthClientIsAnError(t *testing.T) {
	ts, err := TokenSourceFromPath(t.Context(), writeExpiredSignIn(t), nil, slog.New(slog.DiscardHandler))
	require.Error(t, err)
	assert.Nil(t, ts)
	assert.Contains(t, err.Error(), "sign-in HTTP client")
}

// Validates: R-6.8.27
func TestLoginFlows_NilAuthClientIsAnError(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "tokens", "never-written.json")
	logger := slog.New(slog.DiscardHandler)

	ts, err := Login(t.Context(), tokenPath, nil, noopDisplay, logger)
	require.Error(t, err)
	assert.Nil(t, ts)
	assert.Contains(t, err.Error(), "sign-in HTTP client")

	ts, err = LoginWithBrowser(t.Context(), tokenPath, nil, func(context.Context, string) error {
		assert.Fail(t, "a browser login without an auth client must not open the browser")
		return nil
	}, logger)
	require.Error(t, err)
	assert.Nil(t, ts)
	assert.Contains(t, err.Error(), "sign-in HTTP client")
	assert.NoFileExists(t, tokenPath)
}

// Validates: R-6.8.27
func TestLogin_UsesTheInjectedAuthClient(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+signInDeviceCodePath, func(w http.ResponseWriter, _ *http.Request) {
		writeSignInJSON(t, w, testDeviceCodeJSON)
	})
	mux.HandleFunc("POST "+signInGrantPath, func(w http.ResponseWriter, _ *http.Request) {
		writeSignInJSON(t, w, testTokenJSON)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	standIn := newSignInStandIn(t, srv)
	tokenPath := filepath.Join(t.TempDir(), "tokens", "device.json")

	ts, err := Login(t.Context(), tokenPath, &http.Client{Transport: standIn}, noopDisplay, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	got, err := ts.Token()
	require.NoError(t, err)
	assert.Equal(t, "test-access-token", got)
	assert.Equal(t, []string{signInHost + signInDeviceCodePath, signInHost + signInGrantPath}, standIn.requests(),
		"the device-code request and every poll must go through the injected client")
}

// Validates: R-6.8.27
func TestLoginWithBrowser_UsesTheInjectedAuthClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, signInGrantPath, r.URL.Path)
		writeSignInJSON(t, w, testTokenJSON)
	}))
	t.Cleanup(srv.Close)

	standIn := newSignInStandIn(t, srv)
	tokenPath := filepath.Join(t.TempDir(), "tokens", "browser.json")

	// The browser, not the auth client, follows the authorization URL: hand
	// the callback server a code the way the identity platform's redirect
	// would.
	openURL := func(ctx context.Context, authURL string) error {
		parsed, err := url.Parse(authURL)
		require.NoError(t, err)
		assert.Equal(t, signInHost, parsed.Host)

		query := parsed.Query()
		callback := query.Get("redirect_uri") + "?code=test-auth-code&state=" + url.QueryEscape(query.Get("state"))

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, callback, http.NoBody)
		require.NoError(t, err)

		resp, err := doLocalTestRequest(http.DefaultClient, req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		return nil
	}

	ts, err := LoginWithBrowser(t.Context(), tokenPath, &http.Client{Transport: standIn}, openURL, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	got, err := ts.Token()
	require.NoError(t, err)
	assert.Equal(t, "test-access-token", got)
	assert.Equal(t, []string{signInHost + signInGrantPath}, standIn.requests(),
		"the code exchange must go through the injected client")
}
