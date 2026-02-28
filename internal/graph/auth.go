package graph

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/microsoft"

	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

// Azure AD application registered for onedrive-go (public client, multi-tenant + personal).
const defaultClientID = "8efac532-bbe7-4bc5-919c-1443ccab860a"

var defaultScopes = []string{
	"offline_access",
	"Files.ReadWrite.All",
	"User.Read",
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
	cfg := oauthConfig(tokenPath, nil, logger)

	return doLogin(ctx, tokenPath, cfg, display, logger)
}

// doLogin implements the device code flow. Accepts a pre-built oauth2.Config
// so tests can inject a mock endpoint.
func doLogin(
	ctx context.Context,
	tokenPath string,
	cfg *oauth2.Config,
	display func(DeviceAuth),
	logger *slog.Logger,
) (TokenSource, error) {
	logger.Info("starting device code auth flow",
		slog.String("path", tokenPath),
	)

	da, err := cfg.DeviceAuth(ctx)
	if err != nil {
		return nil, fmt.Errorf("graph: device auth request failed: %w", err)
	}

	logger.Info("device code received, waiting for user authorization")

	display(DeviceAuth{
		UserCode:        da.UserCode,
		VerificationURI: da.VerificationURI,
	})

	tok, err := cfg.DeviceAccessToken(ctx, da)
	if err != nil {
		return nil, fmt.Errorf("graph: device code authorization failed: %w", err)
	}

	logger.Info("user authorized, saving token",
		slog.Time("expiry", tok.Expiry),
	)

	if saveErr := tokenfile.Save(tokenPath, tok, nil); saveErr != nil {
		return nil, fmt.Errorf("graph: saving token: %w", saveErr)
	}

	logger.Info("login successful",
		slog.String("path", tokenPath),
		slog.Time("expiry", tok.Expiry),
	)

	src := cfg.TokenSource(ctx, tok)

	return &tokenBridge{src: src, logger: logger}, nil
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
	openURL func(string) error,
	logger *slog.Logger,
) (TokenSource, error) {
	cfg := oauthConfig(tokenPath, nil, logger)

	return doAuthCodeLogin(ctx, tokenPath, cfg, openURL, logger)
}

// doAuthCodeLogin implements the authorization code + PKCE flow. Accepts a
// pre-built oauth2.Config so tests can inject a mock endpoint.
func doAuthCodeLogin(
	ctx context.Context,
	tokenPath string,
	cfg *oauth2.Config,
	openURL func(string) error,
	logger *slog.Logger,
) (TokenSource, error) {
	logger.Info("starting browser auth flow (authorization code + PKCE)",
		slog.String("path", tokenPath),
	)

	// Start the localhost callback server.
	resultCh := make(chan callbackResult, 1)
	mux := http.NewServeMux()

	srv, port, err := startCallbackServer(ctx, mux, resultCh, logger)
	if err != nil {
		return nil, err
	}

	defer shutdownCallbackServer(srv, logger)

	// Configure redirect URL with the actual port. No path suffix — must match
	// the registered "http://localhost" URI exactly (port is ignored by v2.0).
	cfg.RedirectURL = fmt.Sprintf("http://localhost:%d", port)

	// Generate PKCE verifier and random state, build auth URL.
	verifier := oauth2.GenerateVerifier()

	state, err := generateState()
	if err != nil {
		return nil, fmt.Errorf("graph: generating state token: %w", err)
	}

	// Register the callback handler now that we know the state.
	registerCallbackHandler(mux, state, resultCh)

	authURL := cfg.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.S256ChallengeOption(verifier),
	)

	// Open the browser and wait for callback.
	launchBrowser(authURL, openURL, logger)

	code, err := waitForCallback(ctx, resultCh)
	if err != nil {
		return nil, err
	}

	// Exchange and save.
	return exchangeAndSave(ctx, cfg, tokenPath, code, verifier, logger)
}

// startCallbackServer binds to 127.0.0.1:0 and starts an HTTP server with the
// given mux. Returns the server, the port, and any error.
func startCallbackServer(
	ctx context.Context,
	mux *http.ServeMux,
	resultCh chan<- callbackResult,
	logger *slog.Logger,
) (*http.Server, int, error) {
	lc := net.ListenConfig{}

	listener, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, fmt.Errorf("graph: binding localhost listener: %w", err)
	}

	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		listener.Close()
		return nil, 0, fmt.Errorf("graph: listener address is not TCP")
	}

	port := tcpAddr.Port
	logger.Info("callback server listening", slog.Int("port", port))

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: shutdownTimeout,
	}

	go func() {
		if serveErr := srv.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			resultCh <- callbackResult{err: fmt.Errorf("graph: callback server error: %w", serveErr)}
		}
	}()

	return srv, port, nil
}

// registerCallbackHandler adds the callback route to the mux.
// Must be called before the browser redirects back.
func registerCallbackHandler(mux *http.ServeMux, state string, resultCh chan<- callbackResult) {
	mux.HandleFunc("GET "+callbackPath, func(w http.ResponseWriter, r *http.Request) {
		handleOAuthCallback(w, r, state, resultCh)
	})
}

// handleOAuthCallback validates the state, extracts the code, and sends the result.
func handleOAuthCallback(w http.ResponseWriter, r *http.Request, state string, resultCh chan<- callbackResult) {
	// Validate state to prevent CSRF.
	if r.URL.Query().Get("state") != state {
		http.Error(w, "Invalid state parameter", http.StatusBadRequest)
		resultCh <- callbackResult{err: fmt.Errorf("graph: OAuth2 state mismatch (possible CSRF)")}

		return
	}

	// Check for error from the authorization server.
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		desc := r.URL.Query().Get("error_description")
		http.Error(w, "Authorization failed: "+errParam, http.StatusBadRequest)
		resultCh <- callbackResult{err: fmt.Errorf("graph: authorization failed: %s: %s", errParam, desc)}

		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "Missing authorization code", http.StatusBadRequest)
		resultCh <- callbackResult{err: fmt.Errorf("graph: callback missing authorization code")}

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, "<html><body><h1>Authentication successful</h1>"+
		"<p>You can close this window and return to the terminal.</p></body></html>")
	resultCh <- callbackResult{code: code}
}

// shutdownCallbackServer gracefully shuts down the callback HTTP server.
// Accepts an explicit logger instead of using slog.Default() so the caller
// controls logging configuration (B-146).
func shutdownCallbackServer(srv *http.Server, logger *slog.Logger) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		// Best-effort shutdown — log but don't propagate since we're in a defer.
		logger.Warn("callback server shutdown error", slog.String("error", err.Error()))
	}
}

// launchBrowser attempts to open the auth URL. If it fails, prints the URL
// to stderr as a fallback so the user can copy-paste it.
func launchBrowser(authURL string, openURL func(string) error, logger *slog.Logger) {
	logger.Info("opening browser for authorization")

	if openErr := openURL(authURL); openErr != nil {
		logger.Warn("failed to open browser, printing URL",
			slog.String("error", openErr.Error()),
		)

		fmt.Fprintf(os.Stderr, "Open this URL in your browser:\n%s\n", authURL)
	}
}

// waitForCallback blocks until the callback fires or the context is canceled.
func waitForCallback(ctx context.Context, resultCh <-chan callbackResult) (string, error) {
	select {
	case result := <-resultCh:
		if result.err != nil {
			return "", result.err
		}

		return result.code, nil
	case <-ctx.Done():
		return "", fmt.Errorf("graph: browser auth canceled: %w", ctx.Err())
	}
}

// exchangeAndSave exchanges the auth code for a token and persists it.
func exchangeAndSave(
	ctx context.Context,
	cfg *oauth2.Config,
	tokenPath, code, verifier string,
	logger *slog.Logger,
) (TokenSource, error) {
	logger.Info("received authorization code, exchanging for token")

	tok, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("graph: token exchange failed: %w", err)
	}

	logger.Info("token exchange successful", slog.Time("expiry", tok.Expiry))

	if saveErr := tokenfile.Save(tokenPath, tok, nil); saveErr != nil {
		return nil, fmt.Errorf("graph: saving token: %w", saveErr)
	}

	logger.Info("browser login successful",
		slog.String("path", tokenPath),
		slog.Time("expiry", tok.Expiry),
	)

	src := cfg.TokenSource(ctx, tok)

	return &tokenBridge{src: src, logger: logger}, nil
}

// generateState produces a cryptographically random hex string for the OAuth2
// state parameter. Using crypto/rand prevents CSRF attacks.
func generateState() (string, error) {
	b := make([]byte, stateTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return hex.EncodeToString(b), nil
}

// TokenSourceFromPath loads a saved token from the given path and returns a
// TokenSource with auto-refresh and auto-persistence via OnTokenChange.
// Returns ErrNotLoggedIn if no token file exists at the path.
//
// The returned TokenSource binds ctx to the underlying oauth2 token source.
// ctx must outlive the TokenSource — if ctx is canceled, silent token refresh
// will fail. Callers should pass context.Background() for long-lived sessions.
//
// The caller is responsible for computing tokenPath (via config.DriveTokenPath).
// This decouples graph/ from config/ — graph/ has no config import.
func TokenSourceFromPath(ctx context.Context, tokenPath string, logger *slog.Logger) (TokenSource, error) {
	tok, meta, err := tokenfile.Load(tokenPath)
	if err != nil {
		return nil, err
	}

	if tok == nil {
		return nil, ErrNotLoggedIn
	}

	expired := !tok.Expiry.IsZero() && tok.Expiry.Before(time.Now())
	logger.Info("loaded saved token",
		slog.String("path", tokenPath),
		slog.Time("expiry", tok.Expiry),
		slog.Bool("expired", expired),
	)

	cfg := oauthConfig(tokenPath, meta, logger)
	src := cfg.TokenSource(ctx, tok)

	return &tokenBridge{src: src, logger: logger}, nil
}

// Logout removes the saved token file at the given path.
// Returns nil if the token file does not exist (already logged out).
//
// The caller is responsible for computing tokenPath (via config.DriveTokenPath).
// This decouples graph/ from config/ — graph/ has no config import.
func Logout(tokenPath string, logger *slog.Logger) error {
	err := os.Remove(tokenPath)
	if errors.Is(err, fs.ErrNotExist) {
		logger.Info("logout: no token file to remove (already logged out)",
			slog.String("path", tokenPath),
		)

		return nil
	}

	if err != nil {
		return err
	}

	logger.Info("logout: removed token file",
		slog.String("path", tokenPath),
	)

	return nil
}

// oauthConfig builds an oauth2.Config with OnTokenChange wired to persist
// refreshed tokens. meta is captured by the closure so metadata is preserved
// through silent token refreshes.
func oauthConfig(tokenPath string, meta map[string]string, logger *slog.Logger) *oauth2.Config {
	return &oauth2.Config{
		ClientID: defaultClientID,
		Scopes:   defaultScopes,
		Endpoint: microsoft.AzureADEndpoint("common"),
		// Called by ReuseTokenSource after each silent refresh, outside its mutex.
		OnTokenChange: func(tok *oauth2.Token) {
			logger.Info("token refreshed by oauth2 library",
				slog.String("path", tokenPath),
				slog.Time("new_expiry", tok.Expiry),
			)

			if err := tokenfile.Save(tokenPath, tok, meta); err != nil {
				logger.Warn("failed to persist refreshed token",
					slog.String("path", tokenPath),
					slog.String("error", err.Error()),
				)

				return
			}

			logger.Info("persisted refreshed token to disk",
				slog.String("path", tokenPath),
			)
		},
	}
}

// tokenBridge adapts oauth2.TokenSource to graph.TokenSource.
// Logs every token acquisition so refresh activity is visible.
type tokenBridge struct {
	src    oauth2.TokenSource
	logger *slog.Logger
}

func (b *tokenBridge) Token() (string, error) {
	t, err := b.src.Token()
	if err != nil {
		b.logger.Warn("token acquisition failed", slog.String("error", err.Error()))
		return "", fmt.Errorf("graph: obtaining token: %w", err)
	}

	b.logger.Debug("token acquired",
		slog.Time("expiry", t.Expiry),
		slog.Bool("valid", t.Valid()),
	)

	return t.AccessToken, nil
}

// LoadTokenMeta reads just the metadata from a token file.
// Delegates to tokenfile.Load — single loading code path.
// Returns nil metadata (not an error) if the file does not exist.
func LoadTokenMeta(tokenPath string) (map[string]string, error) {
	return tokenfile.ReadMeta(tokenPath)
}

// SaveTokenMeta reads the current token, merges new metadata, and saves.
// New metadata keys overwrite existing ones.
func SaveTokenMeta(tokenPath string, meta map[string]string) error {
	return tokenfile.LoadAndMergeMeta(tokenPath, meta)
}
