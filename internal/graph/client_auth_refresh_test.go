package graph

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// forcingToken models ReuseTokenSource: Token keeps returning the cached
// access token until ForceRefresh redeems the refresh token.
type forcingToken struct {
	mu       stdsync.Mutex
	current  string
	next     string
	forceErr error
	forced   int
}

func (f *forcingToken) Token() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current, nil
}

func (f *forcingToken) ForceRefresh() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forced++
	if f.forceErr != nil {
		return "", f.forceErr
	}
	f.current = f.next
	return f.current, nil
}

func unauthorizedServer(t *testing.T, accept string, body string, header string) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if accept != "" && r.Header.Get("Authorization") == "Bearer "+accept {
			w.Header().Set("Content-Type", "application/json")
			writeClientTestBody(t, w, `{"id":"me"}`)
			return
		}
		if header != "" {
			w.Header().Set("WWW-Authenticate", header)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		writeClientTestBody(t, w, body)
	}))
}

// Validates: R-6.8.14
func TestDoOnce_401_ForcesRefreshWhenCachedTokenIsUnchanged(t *testing.T) {
	t.Parallel()

	srv := unauthorizedServer(t, "fresh", `{"error":{"code":"InvalidAuthenticationToken","message":"revoked"}}`, "")
	defer srv.Close()
	token := &forcingToken{current: "revoked-early", next: "fresh"}
	client := MustNewClient(srv.URL, http.DefaultClient, token, slog.Default(), "test-agent")

	resp, err := client.do(t.Context(), http.MethodGet, "/me", nil)
	require.NoError(t, err)
	closeClientTestResponse(t, resp)
	assert.Equal(t, 1, token.forced, "the unchanged cached token must be force-refreshed, not resent")
}

// Validates: R-6.8.14, R-2.10.69
func TestDoOnce_401_RPSAfterForcedRefreshIsTransient(t *testing.T) {
	t.Parallel()

	srv := unauthorizedServer(t, "", `{"error":{"code":"InvalidAuthenticationToken","message":"Unable to initialize RPS"}}`, "")
	defer srv.Close()
	token := &forcingToken{current: "cached", next: "fresh"}
	client := MustNewClient(srv.URL, http.DefaultClient, token, slog.Default(), "test-agent")

	resp, err := client.do(t.Context(), http.MethodGet, "/me", nil)
	closeClientTestResponse(t, resp)

	require.ErrorIs(t, err, ErrAuthTransient)
	require.NotErrorIs(t, err, ErrUnauthorized, "a valid fresh token is not a sign-in problem")
	var graphErr *GraphError
	require.ErrorAs(t, err, &graphErr)
	assert.Equal(t, http.StatusUnauthorized, graphErr.StatusCode)
}

// Validates: R-6.8.14, R-2.10.69
func TestDoOnce_401_TransientSignatures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		body      string
		header    string
		transient bool
	}{
		{name: "generalException", body: `{"error":{"code":"generalException","message":"x"}}`, transient: true},
		{
			name: "invalid_token without expiry", body: `{"error":{"code":"InvalidAuthenticationToken","message":"x"}}`,
			header: `Bearer realm="", error="invalid_token", error_description="validation failed"`, transient: true,
		},
		{
			name: "expired token", body: `{"error":{"code":"InvalidAuthenticationToken","message":"x"}}`,
			header: `Bearer error="invalid_token", error_description="The access token expired"`,
		},
		{name: "plain rejection", body: `{"error":{"code":"InvalidAuthenticationToken","message":"Access token is empty."}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := unauthorizedServer(t, "", tt.body, tt.header)
			defer srv.Close()
			client := MustNewClient(srv.URL, http.DefaultClient, &forcingToken{current: "a", next: "b"}, slog.Default(), "test-agent")

			resp, err := client.do(t.Context(), http.MethodGet, "/me", nil)
			closeClientTestResponse(t, resp)
			require.Error(t, err)
			assert.Equal(t, tt.transient, errors.Is(err, ErrAuthTransient))
			assert.Equal(t, !tt.transient, errors.Is(err, ErrUnauthorized),
				"a 401 that follows a forced refresh and names no server fault is fatal")
		})
	}
}

// Validates: R-6.8.14
func TestDoOnce_401_RPSWithoutForcedRefreshStaysUnauthorized(t *testing.T) {
	t.Parallel()

	// staticToken cannot prove the credential good, so even an RPS body
	// keeps the conservative sign-in answer.
	srv := unauthorizedServer(t, "", `{"error":{"code":"x","message":"Unable to initialize RPS"}}`, "")
	defer srv.Close()
	client := MustNewClient(srv.URL, http.DefaultClient, staticToken("same"), slog.Default(), "test-agent")

	resp, err := client.do(t.Context(), http.MethodGet, "/me", nil)
	closeClientTestResponse(t, resp)
	require.ErrorIs(t, err, ErrUnauthorized)
}

// Validates: R-6.8.14
func TestDoOnce_401_ForcedRefreshInvalidGrantIsFatal(t *testing.T) {
	t.Parallel()

	srv := unauthorizedServer(t, "", `{"error":{"code":"InvalidAuthenticationToken","message":"Unable to initialize RPS"}}`, "")
	defer srv.Close()
	token := &forcingToken{current: "a", forceErr: errors.Join(ErrLoginExpired, ErrUnauthorized)}
	client := MustNewClient(srv.URL, http.DefaultClient, token, slog.Default(), "test-agent")

	resp, err := client.do(t.Context(), http.MethodGet, "/me", nil)
	closeClientTestResponse(t, resp)
	require.ErrorIs(t, err, ErrUnauthorized)
	require.ErrorIs(t, err, ErrLoginExpired)
}

// Validates: R-6.8.14
func TestTokenBridge_ForceRefreshRedeemsRefreshTokenOncePerBurst(t *testing.T) {
	t.Parallel()

	var redeemed atomic.Int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := redeemed.Add(1)
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-" + strings.Repeat("x", int(n)),
			"refresh_token": "rt",
			"token_type":    "Bearer",
			"expires_in":    3600,
		}))
	}))
	defer tokenSrv.Close()

	cfg := &oauth2.Config{ClientID: "test", Endpoint: oauth2.Endpoint{TokenURL: tokenSrv.URL + "/token"}}
	// The cached token is still valid: ReuseTokenSource alone would never refresh it.
	cached := &oauth2.Token{AccessToken: "cached", RefreshToken: "rt", Expiry: time.Now().Add(time.Hour)}
	clock := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	bridge := newTokenBridge(context.Background(), cfg, cached, slog.Default())
	bridge.now = func() time.Time { return clock }

	got, err := bridge.Token()
	require.NoError(t, err)
	require.Equal(t, "cached", got)

	first, err := bridge.ForceRefresh()
	require.NoError(t, err)
	assert.Equal(t, "fresh-x", first)

	second, err := bridge.ForceRefresh()
	require.NoError(t, err)
	assert.Equal(t, first, second, "a second 401 in the same burst shares the refresh")
	assert.Equal(t, int32(1), redeemed.Load())

	clock = clock.Add(forcedRefreshMinInterval)
	third, err := bridge.ForceRefresh()
	require.NoError(t, err)
	assert.NotEqual(t, first, third)
	assert.Equal(t, int32(2), redeemed.Load())
}

// Validates: R-6.6.34
func TestAADOutcomeError_Table(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		code string
		desc string
		want error
	}{
		{name: "interaction_required", code: "interaction_required", desc: "AADSTS50076: Due to a configuration change ...", want: ErrLoginInteractionRequired},
		{name: "mfa under invalid_grant", code: "invalid_grant", desc: "AADSTS50076: you must use multi-factor authentication", want: ErrLoginInteractionRequired},
		{name: "mfa enrollment", code: "invalid_grant", desc: "AADSTS50079: enroll in MFA", want: ErrLoginInteractionRequired},
		{name: "app access not allowed", code: "invalid_client", desc: "AADSTS65005: The application requires access to a resource", want: ErrLoginAppNotAllowed},
		{name: "consent missing", code: "invalid_grant", desc: "AADSTS65001: The user or administrator has not consented", want: ErrLoginAppNotAllowed},
		{name: "consumers", code: "unauthorized_client", desc: "The client does not exist or is not enabled for consumers.", want: ErrLoginConsumerNotEnabled},
		{name: "expired grant stays unmapped", code: "invalid_grant", desc: "AADSTS70000: grant is expired", want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, aadOutcomeError(tt.code, tt.desc))
		})
	}
}

// Validates: R-6.6.34
func TestTokenBridge_InteractionRequiredIsPlainSignInRequired(t *testing.T) {
	t.Parallel()

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		writeClientTestBody(t, w, `{"error":"interaction_required","error_description":"AADSTS50076: Due to a configuration change made by your administrator, you must use multi-factor authentication."}`)
	}))
	defer tokenSrv.Close()

	cfg := &oauth2.Config{ClientID: "test", Endpoint: oauth2.Endpoint{TokenURL: tokenSrv.URL + "/token"}}
	tok := &oauth2.Token{AccessToken: "old", RefreshToken: "rt", Expiry: time.Now().Add(-time.Hour)}
	bridge := newTokenBridge(context.Background(), cfg, tok, slog.Default())

	_, err := bridge.Token()
	require.ErrorIs(t, err, ErrLoginInteractionRequired)
	require.ErrorIs(t, err, ErrUnauthorized, "it still needs a sign-in, so it takes the sign-in-required path")
	require.ErrorIs(t, err, ErrLoginExpired, "the saved login is no longer usable, not 'rejected while valid'")
}

// Validates: R-6.6.34
func TestBrowserAuthorizationError_AADOutcomesArePlain(t *testing.T) {
	t.Parallel()

	assert.Equal(t, ErrLoginAppNotAllowed, browserAuthorizationError("invalid_client", "AADSTS65005: Misconfigured application"))
	assert.Equal(t, ErrLoginConsumerNotEnabled,
		browserAuthorizationError("unauthorized_client", "The client does not exist or is not enabled for consumers."))
	assert.Equal(t, ErrLoginDeclined, browserAuthorizationError("access_denied", "user canceled"))
}

// Validates: R-6.6.34
func TestDeviceLoginError_AADOutcomesArePlain(t *testing.T) {
	t.Parallel()

	err := deviceLoginError(&oauth2.RetrieveError{
		ErrorCode:        "invalid_grant",
		ErrorDescription: "AADSTS50076: you must use multi-factor authentication",
	}, slog.Default())
	assert.Equal(t, ErrLoginInteractionRequired, err)
}

// Validates: R-2.10.69
func TestDoPreAuth_401IsURLRejectedNotSignIn(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	client := MustNewClient(srv.URL, http.DefaultClient, staticToken("t"), slog.Default(), "test-agent")

	resp, err := client.doPreAuth(t.Context(), "upload chunk", func() (*http.Request, error) {
		return http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+"/upload", http.NoBody)
	})
	closeClientTestResponse(t, resp)

	require.ErrorIs(t, err, ErrPreAuthURLRejected)
	require.NotErrorIs(t, err, ErrUnauthorized, "a rejected transfer URL says nothing about the account's sign-in")
}

// trackedBody records whether the client closed a response body.
type trackedBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *trackedBody) Close() error {
	b.closed.Store(true)
	return nil
}

type unauthorizedTransport struct{ body *trackedBody }

func (u unauthorizedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{}, Body: u.body}, nil
}

// failsAfterFirst hands out one token, then fails: the refresh after the 401.
type failsAfterFirst struct{ calls atomic.Int32 }

func (f *failsAfterFirst) Token() (string, error) {
	if f.calls.Add(1) > 1 {
		return "", errors.New("token endpoint unreachable")
	}
	return "first", nil
}

// Validates: R-6.8.14
func TestDoOnce_401_TokenErrorsCloseTheRejectedBody(t *testing.T) {
	t.Parallel()

	for name, token := range map[string]TokenSource{
		"token refresh fails":  &failsAfterFirst{},
		"forced refresh fails": &forcingToken{current: "a", forceErr: errors.Join(ErrLoginExpired, ErrUnauthorized)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			body := &trackedBody{Reader: strings.NewReader(`{"error":{"code":"InvalidAuthenticationToken"}}`)}
			client := MustNewClient("https://graph.microsoft.com/v1.0",
				&http.Client{Transport: unauthorizedTransport{body: body}}, token, slog.Default(), "test-agent")

			resp, err := client.do(t.Context(), http.MethodGet, "/me", nil)
			closeClientTestResponse(t, resp)
			require.Error(t, err)
			assert.Nil(t, resp)
			assert.True(t, body.closed.Load(), "the 401 body must be closed on every error return")
		})
	}
}
