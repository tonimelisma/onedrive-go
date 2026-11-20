package graph

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// offlineSignInClient is a sign-in client on a machine with no network.
func offlineSignInClient() *http.Client {
	return &http.Client{Timeout: testSignInTimeout, Transport: offlineHost{}}
}

// requireSignInFailure asserts that err is a failure of a request to the
// sign-in service that kept the transport's own error.
func requireSignInFailure(t *testing.T, err error) {
	t.Helper()

	var signIn *SignInRequestError
	require.ErrorAs(t, err, &signIn, "a failed sign-in request names the sign-in service")
	var urlErr *url.Error
	require.ErrorAs(t, err, &urlErr, "the transport's error stays in the chain")
}

// Validates: R-6.8.27
//
// Every request to the sign-in service that fails is marked as that, so a
// caller can name the sign-in service rather than OneDrive: a token refresh
// inside a Graph request (through the not-sent and refresh wrapping), the
// device-code request, its polls, and the browser sign-in's code exchange.
func TestSignInRequestError_MarksEveryRequestToTheSignInService(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)

	t.Run("token refresh inside a Graph request", func(t *testing.T) {
		ts, err := TokenSourceFromPath(writeExpiredSignIn(t), offlineSignInClient(), logger)
		require.NoError(t, err)
		client := MustNewClient("http://localhost", http.DefaultClient, ts, logger, "test-agent")

		resp, err := client.do(t.Context(), http.MethodGet, "/me", nil)
		closeClientTestResponse(t, resp)
		requireSignInFailure(t, err)
		require.ErrorIs(t, err, errRequestNotSent)
	})

	t.Run("device-code request", func(t *testing.T) {
		_, err := Login(t.Context(), filepath.Join(t.TempDir(), "device.json"), offlineSignInClient(), noopDisplay, logger)
		requireSignInFailure(t, err)
	})

	t.Run("device-code poll", func(t *testing.T) {
		cfg := oauthConfig()
		_, err := exchangeDeviceAccessToken(signInContext(t.Context(), offlineSignInClient()), cfg, testDeviceAuthResponse())
		requireSignInFailure(t, err)
	})

	t.Run("browser code exchange", func(t *testing.T) {
		authHTTP := offlineSignInClient()
		cfg := oauthConfig()
		_, err := exchangeAndSave(signInContext(t.Context(), authHTTP), authHTTP, cfg,
			filepath.Join(t.TempDir(), "browser.json"), "code", oauth2.GenerateVerifier(), logger)
		requireSignInFailure(t, err)
	})
}

// Validates: R-6.8.27
//
// A Graph request's own failure is not the sign-in service's.
func TestSignInRequestError_NotOnAGraphRequest(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	baseURL := srv.URL
	srv.Close()

	client := MustNewClient(baseURL, &http.Client{Timeout: time.Minute}, staticToken("t"), slog.New(slog.DiscardHandler), "test-agent")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	resp, err := client.do(ctx, http.MethodGet, "/me", nil)
	closeClientTestResponse(t, resp)
	require.Error(t, err)
	var signIn *SignInRequestError
	require.NotErrorAs(t, err, &signIn)
}
