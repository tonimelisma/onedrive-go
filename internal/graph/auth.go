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

// withAuthClient returns ctx carrying authHTTP as the client oauth2 uses for
// every request to the sign-in service: the device-code request and its
// polls, the code exchange, and each silent refresh of a token source built
// from the returned context. Without it oauth2 falls back to
// http.DefaultClient, which has no timeout (R-6.8.27). A nil authHTTP is a
// caller bug, so it is an error rather than that fallback.
func withAuthClient(ctx context.Context, authHTTP *http.Client) (context.Context, error) {
	if authHTTP == nil {
		return nil, errors.New("graph: sign-in HTTP client must not be nil")
	}

	return context.WithValue(ctx, oauth2.HTTPClient, authHTTP), nil
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
// The returned TokenSource binds ctx to the underlying oauth2 token source.
// ctx must outlive the TokenSource — if ctx is canceled, silent token refresh
// will fail. Callers pass the command's or the daemon's context, never a
// per-request one: the source keeps it for every later refresh.
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
	authCtx, err := withAuthClient(ctx, authHTTP)
	if err != nil {
		return nil, err
	}

	cfg := oauthConfig(tokenPath, logger)

	return doLogin(authCtx, tokenPath, cfg, display, logger)
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
// browser, not authHTTP, follows the authorization URL.
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
	authCtx, err := withAuthClient(ctx, authHTTP)
	if err != nil {
		return nil, err
	}

	cfg := oauthConfig(tokenPath, logger)

	return doAuthCodeLogin(authCtx, tokenPath, cfg, openURL, logger)
}
