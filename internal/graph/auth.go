package graph

import (
	"context"
	"log/slog"
	"time"
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

// DeviceAuth holds the device code response fields that the CLI displays to the user.
type DeviceAuth struct {
	UserCode        string
	VerificationURI string
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
// will fail. Callers should pass context.Background() for long-lived sessions.
//
// The caller is responsible for computing tokenPath (via config.DriveTokenPath).
// This decouples graph/ from config/ — graph/ has no config import.
func Login(
	ctx context.Context,
	tokenPath string,
	display func(DeviceAuth),
	logger *slog.Logger,
) (TokenSource, error) {
	cfg := oauthConfig(tokenPath, logger)

	return doLogin(ctx, tokenPath, cfg, display, logger)
}

// stateTokenBytes is the number of random bytes for the OAuth2 state parameter.
const stateTokenBytes = 16

// callbackPath is the HTTP path the OAuth2 redirect hits on the local server.
// Root path ensures exact match with the registered "http://localhost" redirect
// URI — Microsoft's v2.0 endpoint allows any port but requires path match.
const callbackPath = "/"

// shutdownTimeout is how long to wait for the callback server to drain.
const shutdownTimeout = 5 * time.Second

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
// default browser. If openURL returns an error, the URL is printed to stderr
// so the user can open it manually.
//
// The caller is responsible for computing tokenPath (via config.DriveTokenPath).
// This decouples graph/ from config/ — graph/ has no config import.
func LoginWithBrowser(
	ctx context.Context,
	tokenPath string,
	openURL func(context.Context, string) error,
	logger *slog.Logger,
) (TokenSource, error) {
	cfg := oauthConfig(tokenPath, logger)

	return doAuthCodeLogin(ctx, tokenPath, cfg, openURL, logger)
}
