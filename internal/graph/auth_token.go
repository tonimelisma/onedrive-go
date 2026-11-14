package graph

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	stdsync "sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/microsoft"

	"github.com/tonimelisma/onedrive-go/internal/panicguard"
	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

// TokenSourceFromPath loads a saved token from the given path and returns a
// TokenSource with auto-refresh and auto-persistence via OnTokenChange.
// Returns ErrNotLoggedIn if no token file exists at the path, and an error if
// authHTTP is nil or has no Timeout.
//
// The returned TokenSource keeps no context: each refresh runs under the
// context of the request that needs a token (tokenBridge). authHTTP sends
// every refresh (graphtransport.AuthClient in production), so a stalled token
// endpoint fails the refresh at that client's deadline at the latest.
//
// Production callers reach this through driveops.SessionRuntime, which keeps
// one source per sign-in file (R-6.8.27).
//
// The caller is responsible for computing tokenPath (via config.DriveTokenPath).
// This decouples graph/ from config/ — graph/ has no config import.
func TokenSourceFromPath(
	tokenPath string,
	authHTTP *http.Client,
	logger *slog.Logger,
) (TokenSource, error) {
	if err := checkAuthClient(authHTTP); err != nil {
		return nil, err
	}

	tok, err := loadSavedToken(tokenPath)
	if err != nil {
		return nil, err
	}

	expired := !tok.Expiry.IsZero() && tok.Expiry.Before(time.Now())
	logger.Info("loaded saved token",
		slog.String("path", tokenPath),
		slog.Time("expiry", tok.Expiry),
		slog.Bool("expired", expired),
	)

	return newTokenBridge(oauthConfig(tokenPath, logger), authHTTP, tok, logger), nil
}

// CheckSavedLogin reports whether tokenPath holds a usable saved login,
// without building a token source or logging. It returns ErrNotLoggedIn when
// there is none and a wrapped error when the file cannot be read. Health
// checks use it so a command that then loads the token for real reports
// "loaded saved token" once, not once per check.
func CheckSavedLogin(tokenPath string) error {
	_, err := loadSavedToken(tokenPath)
	return err
}

func loadSavedToken(tokenPath string) (*oauth2.Token, error) {
	tok, err := tokenfile.Load(tokenPath)
	if errors.Is(err, tokenfile.ErrNotFound) {
		return nil, ErrNotLoggedIn
	}

	if err != nil {
		return nil, fmt.Errorf("load token file: %w", err)
	}

	if tok.RefreshToken == "" {
		return nil, ErrNotLoggedIn
	}

	return tok, nil
}

// Logout removes the saved token file at the given path.
// Returns nil if the token file does not exist (already logged out).
//
// The caller is responsible for computing tokenPath (via config.DriveTokenPath).
// This decouples graph/ from config/ — graph/ has no config import.
func Logout(tokenPath string, logger *slog.Logger) error {
	err := tokenfile.Delete(tokenPath)
	if errors.Is(err, tokenfile.ErrNotFound) {
		logger.Info("logout: no token file to remove (already logged out)",
			slog.String("path", tokenPath),
		)

		return nil
	}

	if err != nil {
		return fmt.Errorf("remove token file: %w", err)
	}

	logger.Info("logout: removed token file",
		slog.String("path", tokenPath),
	)

	return nil
}

// oauthConfig builds an oauth2.Config with OnTokenChange wired to persist
// refreshed tokens.
func oauthConfig(tokenPath string, logger *slog.Logger) *oauth2.Config {
	return &oauth2.Config{
		ClientID: defaultClientID,
		Scopes:   defaultScopes(),
		Endpoint: microsoft.AzureADEndpoint("common"),
		// Called on tokenBridge's refresh goroutine once the token endpoint
		// has answered a refresh with a token, before any caller is given it.
		OnTokenChange: func(tok *oauth2.Token) {
			if err := tokenfile.Save(tokenPath, tok); err != nil {
				logger.Warn("couldn't save the refreshed sign-in; the next run may ask you to sign in again. Check the permissions on this file",
					slog.String("path", tokenPath),
					slog.Any("error", err),
				)

				return
			}

			logger.Info("refreshed sign-in",
				slog.String("path", tokenPath),
				slog.Time("expiry", tok.Expiry),
			)
		},
	}
}

// tokenBridge is the graph TokenSource over a saved sign-in. It hands out the
// cached access token while it is valid and otherwise redeems the refresh
// token, under the context of the request that needs the token. Every token
// it hands out is logged at Debug, so refresh activity is visible.
//
// At most one refresh runs at a time (tokenRefresh), and every caller that
// needs a token while it runs waits for that one, each no longer than its own
// context allows. The refresh runs on a goroutine the bridge owns, detached
// from its callers' cancellation, so a caller that stops waiting takes it
// from nobody; when nobody waits any more it still ends, by the sign-in
// client's Timeout (checkAuthClient), and its outcome serves the next caller.
// Only a refresh the token endpoint answered with a token writes the token
// file (OnTokenChange), and it does so before any caller is given the token.
type tokenBridge struct {
	cfg      *oauth2.Config
	authHTTP *http.Client
	logger   *slog.Logger
	now      func() time.Time // injectable for tests

	// mu guards tok, refreshing and lastForced. It is never held while a
	// refresh request is out or while a caller waits for one.
	mu         stdsync.Mutex
	tok        *oauth2.Token
	refreshing *tokenRefresh
	// lastForced is when the last refresh a 401 forced succeeded; it bounds
	// forced refreshes to one per forcedRefreshMinInterval.
	lastForced time.Time
}

// tokenRefresh is one request to the token endpoint, shared by every caller
// waiting for it. tok and err are set before done closes and never change
// after.
type tokenRefresh struct {
	done chan struct{}
	tok  *oauth2.Token
	err  error
	// forced is set, under tokenBridge.mu, when a ForceRefresh starts or joins
	// the refresh: its success starts the forced-refresh interval.
	forced bool
}

// forcedRefreshMinInterval is how long a forced refresh is trusted before
// another 401 may force the next one.
const forcedRefreshMinInterval = 30 * time.Second

func newTokenBridge(cfg *oauth2.Config, authHTTP *http.Client, tok *oauth2.Token, logger *slog.Logger) *tokenBridge {
	return &tokenBridge{
		cfg:      cfg,
		authHTTP: authHTTP,
		logger:   logger,
		now:      time.Now,
		tok:      tok,
	}
}

// Token returns the cached access token while it is valid, and otherwise the
// token a refresh produces, waiting for that refresh no longer than ctx
// allows. A wait ctx ended fails with context.Cause(ctx).
func (b *tokenBridge) Token(ctx context.Context) (string, error) {
	b.mu.Lock()
	if b.tok.Valid() {
		tok := b.tok
		b.mu.Unlock()

		return b.handOut(tok), nil
	}
	refresh, err := b.refreshLocked(ctx, false)
	b.mu.Unlock()
	if err != nil {
		return "", err
	}

	tok, err := b.await(ctx, refresh)
	if err != nil {
		return "", err
	}

	return b.handOut(tok), nil
}

// ForceRefresh redeems the refresh token now even though the cached access
// token has not expired. A refresh driven by expiry alone never redeems a
// valid token, so a 401 against a token the server revoked early (or one
// minted moments before an RPS fault) used to retry with the same rejected
// token and fail fatally (D-072). The new token is persisted through the
// same OnTokenChange hook as any silent refresh. A burst of 401s from
// parallel workers makes one token-endpoint call: each joins the refresh
// under way, and for forcedRefreshMinInterval after a forced refresh
// succeeded, ForceRefresh hands out its token instead of forcing another.
// ctx bounds the wait as for Token.
func (b *tokenBridge) ForceRefresh(ctx context.Context) (string, error) {
	b.mu.Lock()
	if b.refreshing == nil && !b.lastForced.IsZero() && b.now().Sub(b.lastForced) < forcedRefreshMinInterval {
		b.mu.Unlock()

		return b.Token(ctx)
	}
	refresh, err := b.refreshLocked(ctx, true)
	b.mu.Unlock()
	if err != nil {
		return "", err
	}

	tok, err := b.await(ctx, refresh)
	if err != nil {
		return "", err
	}

	b.logger.Debug("token force-refreshed after 401", slog.Time("expiry", tok.Expiry))

	return tok.AccessToken, nil
}

// refreshLocked returns the refresh under way, starting one when there is
// none; b.mu must be held. A caller whose context has already ended starts
// nothing. forced marks the refresh as one a 401 asked for.
func (b *tokenBridge) refreshLocked(ctx context.Context, forced bool) (*tokenRefresh, error) {
	if b.refreshing == nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("refreshing access token: %w", context.Cause(ctx))
		}

		b.refreshing = &tokenRefresh{done: make(chan struct{})}
		// Detached from the caller's cancellation, not from its values: the
		// refresh is everyone's, and the sign-in client's Timeout bounds it.
		go b.redeem(context.WithoutCancel(ctx), b.tok.RefreshToken, b.refreshing)
	}
	if forced {
		b.refreshing.forced = true
	}

	return b.refreshing, nil
}

// await waits for refresh to finish or for ctx to end. Only this caller's
// wait ends with ctx; the refresh goes on for the others.
func (b *tokenBridge) await(ctx context.Context, refresh *tokenRefresh) (*oauth2.Token, error) {
	select {
	case <-refresh.done:
	case <-ctx.Done():
		select {
		case <-refresh.done:
			// It finished at the same moment: its outcome is the answer.
		default:
			b.logger.Debug("stopped waiting for a token refresh", slog.Any("cause", context.Cause(ctx)))
			return nil, fmt.Errorf("refreshing access token: %w", context.Cause(ctx))
		}
	}

	if refresh.err != nil {
		return nil, refresh.err
	}

	return refresh.tok, nil
}

// redeem is the goroutine of one token refresh: it redeems refreshToken and
// gives every waiter the outcome. The token endpoint's answer is saved by
// OnTokenChange inside the oauth2 source's Token call, so a token is on disk
// before any caller gets it. A panic in oauth2 or the sign-in transport is
// recovered at this goroutine's boundary through panicguard: it is logged
// once, with its stack, and every waiter gets the error, so the process goes
// on and no later caller waits on a refresh that will never finish.
func (b *tokenBridge) redeem(ctx context.Context, refreshToken string, refresh *tokenRefresh) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err := panicguard.Recovered(b.logger, "while refreshing the sign-in", recovered)
			b.finish(refresh, nil, fmt.Errorf("refreshing access token: %w", err))
		}
	}()

	tok, err := b.requestToken(ctx, refreshToken)
	b.finish(refresh, tok, err)
}

// requestToken makes a refresh's one request to the token endpoint, under ctx.
func (b *tokenBridge) requestToken(ctx context.Context, refreshToken string) (*oauth2.Token, error) {
	b.logger.Debug("refreshing access token")

	// A token with no access token is not valid, so the source redeems the
	// refresh token at once.
	tok, err := b.cfg.TokenSource(signInContext(ctx, b.authHTTP), &oauth2.Token{RefreshToken: refreshToken}).Token()
	if err != nil {
		return nil, b.tokenError(signInFailure(err))
	}

	return tok, nil
}

// finish records a refresh's outcome and releases its waiters. A token the
// refresh produced replaces the cached one; a failure keeps it, so the next
// caller refreshes again.
func (b *tokenBridge) finish(refresh *tokenRefresh, tok *oauth2.Token, err error) {
	b.mu.Lock()
	if err == nil {
		b.tok = tok
		if refresh.forced {
			b.lastForced = b.now()
		}
	}
	b.refreshing = nil
	refresh.tok, refresh.err = tok, err
	b.mu.Unlock()

	close(refresh.done)
}

// handOut logs a token handed to a caller and returns its access token.
func (b *tokenBridge) handOut(tok *oauth2.Token) string {
	b.logger.Debug("token acquired",
		slog.Time("expiry", tok.Expiry),
		slog.Bool("valid", tok.Valid()),
	)

	return tok.AccessToken
}

// tokenError maps a token-endpoint failure into the graph error vocabulary.
func (b *tokenBridge) tokenError(err error) error {
	var retrieveErr *oauth2.RetrieveError
	if errors.As(err, &retrieveErr) {
		if plain := aadOutcomeError(retrieveErr.ErrorCode, retrieveErr.ErrorDescription); plain != nil {
			// Every one of these means the saved login can no longer be used
			// silently: a new interactive sign-in is the fix. So it also
			// carries ErrLoginExpired and ErrUnauthorized, and status reports
			// the saved login as no longer accepted rather than "rejected
			// with a still-valid login".
			b.logger.Debug("token refresh needs a new sign-in", slog.Any("error", err))
			return fmt.Errorf("refreshing access token: %w: %w: %w: %w", plain, ErrLoginExpired, ErrUnauthorized, err)
		}
	}
	if errors.As(err, &retrieveErr) && retrieveErr.ErrorCode == "invalid_grant" {
		// The caller reports this as "sign-in required"; logging it as a
		// warning as well would only repeat the same fact less clearly.
		b.logger.Debug("saved login rejected", slog.Any("error", err))
		return fmt.Errorf("refreshing access token: %w: %w: %w", ErrLoginExpired, ErrUnauthorized, err)
	}

	// Debug only: the error is returned, and the command or sync layer
	// reports it once (as a network or sign-in problem). Sync workers ask for
	// a token per request, so a Warn here repeated on every request while
	// offline with an expired access token.
	b.logger.Debug("token acquisition failed", slog.Any("error", err))
	return fmt.Errorf("refreshing access token: %w", err)
}
