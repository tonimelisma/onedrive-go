package graph

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"golang.org/x/oauth2"

	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

// OAuth error codes the token and authorization endpoints return for
// user-side sign-in outcomes (RFC 6749 section 4.1.2.1, RFC 8628 section 3.5).
const (
	oauthErrAccessDenied          = "access_denied"
	oauthErrAuthorizationDeclined = "authorization_declined"
	oauthErrExpiredToken          = "expired_token"
)

type (
	deviceAuthRequester  func(context.Context, *oauth2.Config) (*oauth2.DeviceAuthResponse, error)
	deviceTokenExchanger func(context.Context, *oauth2.Config, *oauth2.DeviceAuthResponse) (*oauth2.Token, error)
)

func requestDeviceAuth(ctx context.Context, cfg *oauth2.Config) (*oauth2.DeviceAuthResponse, error) {
	auth, err := cfg.DeviceAuth(ctx)
	if err != nil {
		return nil, fmt.Errorf("device auth request: %w", err)
	}

	return auth, nil
}

func exchangeDeviceAccessToken(
	ctx context.Context,
	cfg *oauth2.Config,
	auth *oauth2.DeviceAuthResponse,
) (*oauth2.Token, error) {
	tok, err := cfg.DeviceAccessToken(ctx, auth)
	if err != nil {
		return nil, fmt.Errorf("device access token: %w", err)
	}

	return tok, nil
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
	return doLoginWithFlow(ctx, tokenPath, cfg, display, logger, requestDeviceAuth, exchangeDeviceAccessToken)
}

func doLoginWithFlow(
	ctx context.Context,
	tokenPath string,
	cfg *oauth2.Config,
	display func(DeviceAuth),
	logger *slog.Logger,
	requestAuth deviceAuthRequester,
	exchangeToken deviceTokenExchanger,
) (TokenSource, error) {
	logger.Info("starting device code auth flow",
		slog.String("path", tokenPath),
	)

	da, err := requestAuth(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("graph: device auth request failed: %w", err)
	}

	logger.Info("device code received, waiting for user authorization")

	display(DeviceAuth{
		UserCode:        da.UserCode,
		VerificationURI: da.VerificationURI,
		Expiry:          da.Expiry,
	})

	tok, err := exchangeToken(ctx, cfg, da)
	if err != nil {
		return nil, deviceLoginError(err, logger)
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

	return newTokenBridge(ctx, cfg, tok, logger), nil
}

// deviceLoginError turns the token endpoint's answer for the common user-side
// outcomes (code expired, sign-in declined) into plain sentinel errors, so the
// default Error line does not carry raw OAuth codes and AADSTS text. The raw
// error stays in a Debug record. Anything else keeps its full chain.
func deviceLoginError(err error, logger *slog.Logger) error {
	var retrieveErr *oauth2.RetrieveError
	if errors.As(err, &retrieveErr) {
		switch retrieveErr.ErrorCode {
		case oauthErrExpiredToken:
			logger.Debug("device code expired before authorization", slog.String("error", err.Error()))
			return ErrLoginCodeExpired
		case oauthErrAccessDenied, oauthErrAuthorizationDeclined:
			logger.Debug("device code authorization declined", slog.String("error", err.Error()))
			return ErrLoginDeclined
		}
		if plain := aadOutcomeError(retrieveErr.ErrorCode, retrieveErr.ErrorDescription); plain != nil {
			logger.Debug("device code authorization refused", slog.String("error", err.Error()))
			return plain
		}
	}

	return fmt.Errorf("graph: device code authorization failed: %w", err)
}
