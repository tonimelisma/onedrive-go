package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/microsoft"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

// Azure AD application registered for onedrive-go (public client, multi-tenant + personal).
const defaultClientID = "6f6f82bc-3154-4a0b-b399-316ab64594d0"

// tokenFilePerms restricts token files to owner-only read/write.
const tokenFilePerms = 0o600

// dirPerms is used when creating the tokens directory.
const dirPerms = 0o700

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

// Login performs the device code OAuth2 flow for a profile:
//  1. Requests a device code from Microsoft
//  2. Calls display so the CLI can show the user code and verification URL
//  3. Polls until the user authorizes (blocking, respects ctx cancellation)
//  4. Saves the token to disk
//  5. Returns a TokenSource for use with Client
func Login(
	ctx context.Context,
	profile string,
	display func(DeviceAuth),
	logger *slog.Logger,
) (TokenSource, error) {
	tokenPath := config.ProfileTokenPath(profile)
	if tokenPath == "" {
		return nil, fmt.Errorf("graph: cannot determine token path for profile %q", profile)
	}

	cfg := oauthConfig(tokenPath, logger)

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
	da, err := cfg.DeviceAuth(ctx)
	if err != nil {
		return nil, fmt.Errorf("graph: device auth request failed: %w", err)
	}

	display(DeviceAuth{
		UserCode:        da.UserCode,
		VerificationURI: da.VerificationURI,
	})

	tok, err := cfg.DeviceAccessToken(ctx, da)
	if err != nil {
		return nil, fmt.Errorf("graph: device code authorization failed: %w", err)
	}

	if saveErr := saveToken(tokenPath, tok); saveErr != nil {
		return nil, fmt.Errorf("graph: saving token: %w", saveErr)
	}

	logger.Info("login successful", slog.String("path", tokenPath))

	src := cfg.TokenSource(ctx, tok)

	return &tokenBridge{src: src}, nil
}

// TokenSourceFromProfile loads a saved token and returns a TokenSource with
// auto-refresh and auto-persistence via OnTokenChange.
// Returns ErrNotLoggedIn if no token file exists for the profile.
func TokenSourceFromProfile(ctx context.Context, profile string, logger *slog.Logger) (TokenSource, error) {
	tokenPath := config.ProfileTokenPath(profile)
	if tokenPath == "" {
		return nil, fmt.Errorf("graph: cannot determine token path for profile %q", profile)
	}

	return tokenSourceFromPath(ctx, tokenPath, logger)
}

// tokenSourceFromPath loads a token from the given path and returns a TokenSource.
func tokenSourceFromPath(ctx context.Context, tokenPath string, logger *slog.Logger) (TokenSource, error) {
	tok, err := loadToken(tokenPath)
	if err != nil {
		return nil, err
	}

	if tok == nil {
		return nil, ErrNotLoggedIn
	}

	logger.Info("loaded saved token",
		slog.String("path", tokenPath),
		slog.Time("expiry", tok.Expiry),
	)

	cfg := oauthConfig(tokenPath, logger)
	src := cfg.TokenSource(ctx, tok)

	return &tokenBridge{src: src}, nil
}

// Logout removes the saved token file for a profile.
// Returns nil if the token file does not exist (already logged out).
func Logout(profile string) error {
	tokenPath := config.ProfileTokenPath(profile)
	if tokenPath == "" {
		return fmt.Errorf("graph: cannot determine token path for profile %q", profile)
	}

	return logout(tokenPath)
}

// logout removes the token file at the given path.
// Returns nil if the file does not exist (idempotent).
func logout(tokenPath string) error {
	err := os.Remove(tokenPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	return err
}

// oauthConfig builds an oauth2.Config with OnTokenChange wired to persist
// refreshed tokens. This is the key integration with the oauth2 fork.
func oauthConfig(tokenPath string, logger *slog.Logger) *oauth2.Config {
	return &oauth2.Config{
		ClientID: defaultClientID,
		Scopes:   defaultScopes,
		Endpoint: microsoft.AzureADEndpoint("common"),
		// Called by ReuseTokenSource after each silent refresh, outside its mutex.
		OnTokenChange: func(tok *oauth2.Token) {
			if err := saveToken(tokenPath, tok); err != nil {
				logger.Warn("failed to persist refreshed token",
					slog.String("path", tokenPath),
					slog.String("error", err.Error()),
				)
			}
		},
	}
}

// tokenBridge adapts oauth2.TokenSource to graph.TokenSource.
type tokenBridge struct {
	src oauth2.TokenSource
}

func (b *tokenBridge) Token() (string, error) {
	t, err := b.src.Token()
	if err != nil {
		return "", fmt.Errorf("graph: obtaining token: %w", err)
	}

	return t.AccessToken, nil
}

// loadToken reads a saved oauth2.Token from disk.
// Returns (nil, nil) if the file does not exist.
func loadToken(path string) (*oauth2.Token, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("graph: reading token file: %w", err)
	}

	var tok oauth2.Token
	if err := json.Unmarshal(data, &tok); err != nil {
		return nil, fmt.Errorf("graph: decoding token file: %w", err)
	}

	return &tok, nil
}

// saveToken writes an oauth2.Token to disk atomically (write-to-temp + rename)
// with 0600 permissions. Never logs token values (architecture.md §9.2).
func saveToken(path string, tok *oauth2.Token) error {
	data, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return fmt.Errorf("graph: encoding token: %w", err)
	}

	dir := filepath.Dir(path)
	if mkErr := os.MkdirAll(dir, dirPerms); mkErr != nil {
		return fmt.Errorf("graph: creating token directory: %w", mkErr)
	}

	// Atomic write: temp file in the same directory, then rename.
	// Same directory guarantees same filesystem for rename(2).
	tmp, err := os.CreateTemp(dir, ".token-*.tmp")
	if err != nil {
		return fmt.Errorf("graph: creating temp file: %w", err)
	}

	tmpPath := tmp.Name()

	// Clean up temp file on any error path.
	success := false
	defer func() {
		if !success {
			os.Remove(tmpPath)
		}
	}()

	if err := os.Chmod(tmpPath, tokenFilePerms); err != nil {
		tmp.Close()
		return fmt.Errorf("graph: setting token file permissions: %w", err)
	}

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("graph: writing token file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("graph: closing token file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("graph: renaming token file: %w", err)
	}

	success = true

	return nil
}
