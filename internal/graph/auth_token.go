package graph

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/microsoft"

	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

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

	expired := !tok.Expiry.IsZero() && tok.Expiry.Before(time.Now())
	logger.Info("loaded saved token",
		slog.String("path", tokenPath),
		slog.Time("expiry", tok.Expiry),
		slog.Bool("expired", expired),
	)

	cfg := oauthConfig(tokenPath, logger)
	src := cfg.TokenSource(ctx, tok)

	return &tokenBridge{src: src, logger: logger}, nil
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
		// Called by ReuseTokenSource after each silent refresh, outside its mutex.
		OnTokenChange: func(tok *oauth2.Token) {
			logger.Info("token refreshed by oauth2 library",
				slog.String("path", tokenPath),
				slog.Time("new_expiry", tok.Expiry),
			)

			if err := tokenfile.Save(tokenPath, tok); err != nil {
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
