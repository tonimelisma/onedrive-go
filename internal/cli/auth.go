package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newLoginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate with OneDrive",
		Long: `Authenticate with OneDrive using the device code flow (default) or browser-based
authorization code flow (--browser).

Discovers your account type (personal/business) and organization automatically.
Creates or updates the config file with the new drive section.

The --browser flag opens your default browser for authentication, which can be
useful when the device code flow is blocked by organizational policies.`,
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		RunE:        runLogin,
	}

	cmd.Flags().Bool("browser", false, "use browser-based auth (authorization code + PKCE) instead of device code")

	return cmd
}

func newLogoutCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Remove saved authentication token and drive config",
		Long: `Remove the saved authentication token and drive config sections for an account.
State databases are kept so the drive can be re-added without a full re-sync.

With --purge, state databases are also deleted.

If only one account is configured, it is selected automatically.
Otherwise, use --account to specify which account to log out.`,
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		RunE:        runLogout,
	}

	cmd.Flags().Bool("purge", false, "also delete state databases")

	return cmd
}

func newWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:         "whoami",
		Short:       "Display the authenticated user and drive info",
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		RunE:        runWhoami,
	}
}

// runLogin implements the discovery-based login flow per accounts.md section 9:
// device code auth -> /me -> /me/drive -> /me/organization -> construct canonical ID -> config.
func runLogin(cmd *cobra.Command, _ []string) error {
	useBrowser, err := cmd.Flags().GetBool("browser")
	if err != nil {
		return fmt.Errorf("reading --browser flag: %w", err)
	}

	return runLoginWithContext(cmd.Context(), mustCLIContext(cmd.Context()), useBrowser)
}

// runLogout removes the authentication token for an account. Identifies the
// account via --account flag or auto-selects if only one account exists.
func runLogout(cmd *cobra.Command, _ []string) error {
	purge, err := cmd.Flags().GetBool("purge")
	if err != nil {
		return fmt.Errorf("reading --purge flag: %w", err)
	}

	return runLogoutWithContext(mustCLIContext(cmd.Context()), purge)
}

func runWhoami(cmd *cobra.Command, _ []string) error {
	return runWhoamiWithContext(cmd.Context(), mustCLIContext(cmd.Context()))
}
