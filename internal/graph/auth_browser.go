package graph

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"

	"golang.org/x/oauth2"

	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

// doAuthCodeLogin implements the authorization code + PKCE flow. Accepts a
// pre-built oauth2.Config so tests can inject a mock endpoint.
func doAuthCodeLogin(
	ctx context.Context,
	tokenPath string,
	cfg *oauth2.Config,
	openURL func(context.Context, string) error,
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

	defer shutdownCallbackServer(ctx, srv, logger)

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
	launchBrowser(ctx, authURL, openURL, logger)

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
		baseErr := fmt.Errorf("graph: listener address is not TCP")
		if closeErr := listener.Close(); closeErr != nil {
			return nil, 0, errors.Join(baseErr, fmt.Errorf("graph: closing callback listener: %w", closeErr))
		}

		return nil, 0, baseErr
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
	if _, err := io.WriteString(w, "<html><body><h1>Authentication successful</h1>"+
		"<p>You can close this window and return to the terminal.</p></body></html>"); err != nil {
		resultCh <- callbackResult{err: fmt.Errorf("graph: writing callback success page: %w", err)}

		return
	}
	resultCh <- callbackResult{code: code}
}

// shutdownCallbackServer gracefully shuts down the callback HTTP server.
// Accepts an explicit logger instead of using slog.Default() so the caller
// controls logging configuration (B-146).
func shutdownCallbackServer(ctx context.Context, srv *http.Server, logger *slog.Logger) {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		// Best-effort shutdown — log but don't propagate since we're in a defer.
		logger.Warn("callback server shutdown error", slog.String("error", err.Error()))
	}
}

// launchBrowser attempts to open the auth URL. The openURL callback owns any
// user-facing fallback because the CLI owns terminal output formatting.
func launchBrowser(ctx context.Context, authURL string, openURL func(context.Context, string) error, logger *slog.Logger) {
	logger.Info("opening browser for authorization")

	if openErr := openURL(ctx, authURL); openErr != nil {
		logger.Warn("failed to open browser",
			slog.String("error", openErr.Error()),
		)
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

	if saveErr := tokenfile.Save(tokenPath, tok); saveErr != nil {
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
		return "", fmt.Errorf("read random state token: %w", err)
	}

	return hex.EncodeToString(b), nil
}
