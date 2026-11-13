package graph

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/oauth2"
)

// Azure AD application registered for onedrive-go (public client, multi-tenant + personal).
const defaultClientID = "8efac532-bbe7-4bc5-919c-1443ccab860a"

func defaultScopes() []string {
	return []string{
		"offline_access",
		"Files.ReadWrite.All",
		"User.Read",
	}
}

// checkAuthClient refuses a sign-in HTTP client that could wait forever. A nil
// one is a caller bug: oauth2 would fall back to http.DefaultClient, which
// has no timeout (R-6.8.27). One without a Timeout is refused for the same
// reason: a token refresh outlives the callers that stop waiting for it, and
// only the client's Timeout ends it then.
func checkAuthClient(authHTTP *http.Client) error {
	if authHTTP == nil {
		return errors.New("graph: sign-in HTTP client must not be nil")
	}
	if authHTTP.Timeout <= 0 {
		return errors.New("graph: sign-in HTTP client must have a timeout")
	}

	return nil
}

// signInContext returns ctx carrying authHTTP as the client oauth2 uses for
// its requests to the sign-in service: the device-code request and its
// polls, the code exchange, or one token refresh.
func signInContext(ctx context.Context, authHTTP *http.Client) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, authHTTP)
}

// SignInRequestError is the failure of a request to the Microsoft identity
// platform, the sign-in service, rather than to Graph: a token refresh, the
// device-code request and its polls, or the browser sign-in's code exchange.
// It wraps the request's own error, so the chain still answers errors.As for
// *url.Error and *oauth2.RetrieveError and errors.Is for a context's error,
// and it stays in the chain through a refresh's wrapping and a request not
// sent. A caller asks for it with errors.As to name the sign-in service
// instead of OneDrive.
type SignInRequestError struct {
	Err error
}

func (e *SignInRequestError) Error() string { return e.Err.Error() }

func (e *SignInRequestError) Unwrap() error { return e.Err }

// signInFailure marks err, the failure of a request to the sign-in service,
// as a *SignInRequestError. Every sign-in request's failure passes through
// it: tokenBridge.redeem, requestDeviceAuth, exchangeDeviceAccessToken and
// exchangeAndSave.
func signInFailure(err error) error {
	if err == nil {
		return nil
	}

	return &SignInRequestError{Err: err}
}

// DeviceAuth holds the device code response fields that the CLI displays to the user.
type DeviceAuth struct {
	UserCode        string
	VerificationURI string
	// Expiry is when the code stops working; zero when Microsoft omitted it.
	Expiry time.Time
}

// Login performs the device code OAuth2 flow:
//  1. Requests a device code from Microsoft
//  2. Calls display so the CLI can show the user code and verification URL
//  3. Polls until the user authorizes (blocking, respects ctx cancellation)
//  4. Saves the token to disk at tokenPath
//  5. Returns a TokenSource for use with Client
//
// ctx bounds the sign-in flow only. The returned TokenSource keeps no
// context: each later refresh runs under the context of the request that
// needs a token.
//
// authHTTP sends every request to the sign-in service, the polls and the
// returned source's refreshes included; callers pass graphtransport.AuthClient.
//
// The caller is responsible for computing tokenPath (via config.DriveTokenPath).
// This decouples graph/ from config/ — graph/ has no config import.
func Login(
	ctx context.Context,
	tokenPath string,
	authHTTP *http.Client,
	display func(DeviceAuth),
	logger *slog.Logger,
) (TokenSource, error) {
	return doLogin(ctx, tokenPath, authHTTP, oauthConfig(tokenPath, logger), display, logger)
}

// stateTokenBytes is the number of random bytes for the OAuth2 state parameter.
const stateTokenBytes = 16

// callbackPath is the HTTP path the OAuth2 redirect hits on the local server.
// Root path ensures exact match with the registered "http://localhost" redirect
// URI — Microsoft's v2.0 endpoint allows any port but requires path match.
const callbackPath = "/"

// callbackShutdownTimeout is how long to wait for the callback server to drain.
const callbackShutdownTimeout = 5 * time.Second

// callbackHeaderReadTimeout bounds how long the callback server waits for the
// headers of a request on an accepted connection, so a client that connects
// and sends nothing cannot hold a server goroutine.
const callbackHeaderReadTimeout = 5 * time.Second

// callbackResult carries the authorization code or error from the callback handler.
type callbackResult struct {
	code string
	err  error
}

// LoginWithBrowser performs the authorization code + PKCE flow:
//  1. Binds a localhost HTTP server on a random port
//  2. Opens the browser to Microsoft's authorization endpoint
//  3. Receives the callback with the authorization code
//  4. Exchanges the code for tokens using PKCE
//  5. Saves the token to disk at tokenPath
//  6. Returns a TokenSource for use with Client
//
// openURL is called with the authorization URL; the CLI uses it to launch the
// default browser and owns any user-facing fallback output if opening fails.
//
// authHTTP sends the code exchange and the returned source's refreshes; the
// browser, not authHTTP, follows the authorization URL. As for Login, ctx
// bounds the sign-in flow only.
//
// The caller is responsible for computing tokenPath (via config.DriveTokenPath).
// This decouples graph/ from config/ — graph/ has no config import.
func LoginWithBrowser(
	ctx context.Context,
	tokenPath string,
	authHTTP *http.Client,
	openURL func(context.Context, string) error,
	logger *slog.Logger,
) (TokenSource, error) {
	return doAuthCodeLogin(ctx, tokenPath, authHTTP, oauthConfig(tokenPath, logger), openURL, logger)
}
