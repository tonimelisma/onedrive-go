package graph

import (
	"context"
	"fmt"
	"log/slog"

	"golang.org/x/oauth2"

	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

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

	if saveErr := tokenfile.Save(tokenPath, tok); saveErr != nil {
		return nil, fmt.Errorf("graph: saving token: %w", saveErr)
	}

	logger.Info("login successful",
		slog.String("path", tokenPath),
		slog.Time("expiry", tok.Expiry),
	)

	src := cfg.TokenSource(ctx, tok)

	return &tokenBridge{src: src, logger: logger}, nil
}
