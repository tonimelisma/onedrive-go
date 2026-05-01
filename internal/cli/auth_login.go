package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// pendingTokenFile is the filename for the temporary token saved during login
// before the canonical ID is known. This solves the chicken-and-egg problem:
// we need a token to call /me, but the token filename depends on /me results.
const pendingTokenFile = ".token-pending.json"

// tokenDirPerms is the permission mode for token directories (owner only).
const tokenDirPerms = 0o700

// graphDriveTypeDocumentLibrary is the Graph API drive type for SharePoint libraries.
const graphDriveTypeDocumentLibrary = "documentLibrary"

const (
	httpScheme  = "http"
	httpsScheme = "https"
)

const (
	goosDarwin = "darwin"
	goosLinux  = "linux"
)

// findTokenFallback tries personal and business canonical ID prefixes
// and returns whichever one has a token file on disk. Falls back to
// "personal:" if neither exists, since personal is the most common case.
// Logs the probe results so --debug reveals which token path was selected.
func findTokenFallback(account string, logger *slog.Logger) driveid.CanonicalID {
	personalID := driveid.MustCanonicalID("personal:" + account)

	personalPath := config.DriveTokenPath(personalID)
	if personalPath != "" {
		if managedPathExists(personalPath) {
			logger.Debug("token fallback: found personal token", "path", personalPath)

			return personalID
		}
	}

	businessID := driveid.MustCanonicalID("business:" + account)

	businessPath := config.DriveTokenPath(businessID)
	if businessPath != "" {
		if managedPathExists(businessPath) {
			logger.Debug("token fallback: found business token", "path", businessPath)

			return businessID
		}
	}

	// Default to personal if neither exists (best guess for most users).
	logger.Debug("token fallback: no token found, defaulting to personal", "account", account)

	return personalID
}

// pendingTokenPath returns the path for the temporary token file used during
// login before the canonical ID is discovered.
func pendingTokenPath() string {
	return filepath.Join(config.DefaultDataDir(), pendingTokenFile)
}

// openBrowser attempts to open a URL in the user's default browser.
// Uses "open" on macOS and "xdg-open" on Linux. Returns an error if the
// browser command fails or the platform is unsupported.
func openBrowser(ctx context.Context, rawURL string) error {
	validatedURL, err := validateBrowserAuthURL(rawURL)
	if err != nil {
		return err
	}

	command, err := browserOpenCommand(runtime.GOOS)
	if err != nil {
		return err
	}

	// Command name is selected from a fixed allowlist and the URL has already
	// been validated against the Microsoft auth hosts.
	cmd := exec.CommandContext(ctx, command, validatedURL) //nolint:gosec // Fixed browser command with validated auth URL.

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start browser command: %w", err)
	}

	return nil
}

func browserOpenCommand(goos string) (string, error) {
	switch goos {
	case goosDarwin:
		return "open", nil
	case goosLinux:
		return "xdg-open", nil
	default:
		return "", fmt.Errorf("unsupported platform %s: open the URL manually", goos)
	}
}

func validateBrowserAuthURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parsing browser auth URL: %w", err)
	}

	if parsed.User != nil {
		return "", fmt.Errorf("browser auth URL must not contain userinfo")
	}

	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", fmt.Errorf("browser auth URL host is empty")
	}

	if isLoopbackBrowserHost(host) {
		if parsed.Scheme != httpScheme && parsed.Scheme != httpsScheme {
			return "", fmt.Errorf("browser auth URL loopback host must use http or https")
		}

		return parsed.String(), nil
	}

	if parsed.Scheme != httpsScheme {
		return "", fmt.Errorf("browser auth URL must use https")
	}

	if !browserHostAllowed(host) {
		return "", fmt.Errorf("browser auth URL host %q is not allowed", host)
	}

	return parsed.String(), nil
}

func browserHostAllowed(host string) bool {
	for _, allowedHost := range []string{
		"login.microsoftonline.com",
		"login.microsoftonline.us",
		"login.partner.microsoftonline.cn",
		"login.live.com",
	} {
		if host == allowedHost || strings.HasSuffix(host, "."+allowedHost) {
			return true
		}
	}

	return false
}

func isLoopbackBrowserHost(host string) bool {
	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// discoverAccount calls /me and /me/drive to build the canonical drive ID. Work
// or school accounts also read /me/organization for display naming; personal
// Microsoft accounts skip that endpoint because Graph does not support it.
// Returns the canonical ID, user profile, org display name, and the primary
// drive's Graph API ID.
func discoverAccount(
	ctx context.Context,
	ts graph.TokenSource,
	logger *slog.Logger,
	runtime *driveops.SessionRuntime,
) (driveid.CanonicalID, *graph.User, string, driveid.ID, error) {
	client, err := newGraphClientWithHTTP(runtime.GraphBaseURL, runtime.BootstrapMeta(), ts, logger)
	if err != nil {
		return driveid.CanonicalID{}, nil, "", driveid.ID{}, err
	}

	// GET /me -> email, user GUID
	user, err := client.Me(ctx)
	if err != nil {
		return driveid.CanonicalID{}, nil, "", driveid.ID{}, fmt.Errorf("fetching user profile: %w", err)
	}

	logger.Info("discovered user", "email", user.Email, "display_name", user.DisplayName)

	// GET /me/drive (singular) -> primary drive ID and type.
	// Must use /me/drive, NOT /me/drives. The /me/drives endpoint returns all
	// drives including phantom system drives (Photos face crops, album metadata)
	// that Microsoft creates on personal accounts. These appear in non-deterministic
	// order and return HTTP 400 "ObjectHandle is Invalid" when accessed.
	primary, err := client.PrimaryDrive(ctx)
	if err != nil {
		return driveid.CanonicalID{}, nil, "", driveid.ID{}, fmt.Errorf("fetching primary drive: %w", err)
	}

	driveType := primary.DriveType
	logger.Info("discovered drive type", "drive_type", driveType)

	// Warn on unknown drive types — don't block login, but flag it for debugging.
	// Known types: "personal", "business", "documentLibrary" (SharePoint).
	switch driveType {
	case driveid.DriveTypePersonal, driveid.DriveTypeBusiness, graphDriveTypeDocumentLibrary:
	default:
		logger.Warn("unknown drive type from Graph API, proceeding anyway",
			"drive_type", driveType)
	}

	primaryDriveID := primary.ID
	logger.Info("discovered primary drive", "drive_id", primaryDriveID.String())

	// GET /me/organization -> org display name. Microsoft Graph documents this
	// endpoint as unsupported for delegated personal Microsoft accounts.
	var orgName string

	if driveType == driveid.DriveTypePersonal {
		logger.Debug("skipping organization discovery for personal account")
	} else {
		org, orgErr := client.Organization(ctx)
		if orgErr != nil {
			logger.Warn("failed to fetch organization, continuing without org name", "error", orgErr)
		} else if org.DisplayName != "" {
			orgName = org.DisplayName
			logger.Info("discovered organization", "org_name", orgName)
		}
	}

	cid, err := driveid.Construct(driveType, user.Email)
	if err != nil {
		return driveid.CanonicalID{}, nil, "", driveid.ID{}, fmt.Errorf("constructing canonical ID: %w", err)
	}

	logger.Info("constructed canonical ID", "canonical_id", cid.String())

	return cid, user, orgName, primaryDriveID, nil
}

// moveToken renames the pending token file to its final canonical path.
// Creates the destination directory if needed.
func moveToken(src, dst string) error {
	dir := filepath.Dir(dst)
	if err := localpath.MkdirAll(dir, tokenDirPerms); err != nil {
		baseErr := fmt.Errorf("creating token directory: %w", err)
		if cleanupErr := removePathIfExists(src); cleanupErr != nil {
			return errors.Join(baseErr, cleanupErr)
		}

		return baseErr
	}

	if err := localpath.Rename(src, dst); err != nil {
		baseErr := fmt.Errorf("moving token to final path: %w", err)
		if cleanupErr := removePathIfExists(src); cleanupErr != nil {
			return errors.Join(baseErr, cleanupErr)
		}

		return baseErr
	}

	return nil
}

// printLoginSuccess prints the user-facing login output. Format differs
// for personal vs. business accounts per accounts.md section 9.
func printLoginSuccess(w io.Writer, driveType, email, orgName, canonicalID, syncDir string) error {
	switch driveType {
	case driveid.DriveTypePersonal:
		if err := writef(w, "Signed in as %s (personal account).\n", email); err != nil {
			return err
		}

		return writef(w, "Drive added: %s -> %s\n", canonicalID, syncDir)
	case driveid.DriveTypeBusiness:
		orgLabel := orgName
		if orgLabel == "" {
			orgLabel = "business account"
		}

		if err := writef(w, "Signed in as %s (%s).\n", email, orgLabel); err != nil {
			return err
		}
		if err := writef(w, "Drive added: %s -> %s\n", canonicalID, syncDir); err != nil {
			return err
		}
		if err := writeln(w); err != nil {
			return err
		}
		if err := writeln(w, "You also have access to SharePoint libraries."); err != nil {
			return err
		}

		return writeln(w, "Run 'onedrive-go drive search <term>' to find and add them.")
	default:
		if err := writef(w, "Signed in as %s.\n", email); err != nil {
			return err
		}

		return writef(w, "Drive added: %s -> %s\n", canonicalID, syncDir)
	}
}
