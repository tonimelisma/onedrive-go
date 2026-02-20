package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func newLoginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Authenticate with OneDrive using device code flow",
		RunE:  runLogin,
	}
}

func newLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove saved authentication token",
		RunE:  runLogout,
	}
}

func newWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Display the authenticated user and drive info",
		RunE:  runWhoami,
	}
}

// authTokenPath derives the token file path for auth commands. Auth commands
// bypass config loading (PersistentPreRunE skips them) because login must work
// before any config file or drive section exists. The --drive flag provides the
// canonical ID used to locate the token file.
func authTokenPath() (string, error) {
	if flagDrive == "" {
		return "", fmt.Errorf("--drive is required (e.g., --drive personal:user@example.com)")
	}

	tokenPath := config.DriveTokenPath(flagDrive)
	if tokenPath == "" {
		return "", fmt.Errorf("cannot determine token path for drive %q", flagDrive)
	}

	return tokenPath, nil
}

func runLogin(_ *cobra.Command, _ []string) error {
	logger := buildLogger()
	ctx := context.Background()

	tokenPath, err := authTokenPath()
	if err != nil {
		return err
	}

	logger.Info("login started", "drive", flagDrive, "token_path", tokenPath)

	_, err = graph.Login(ctx, tokenPath, func(da graph.DeviceAuth) {
		// Device code prompts must always be visible — not suppressed by --quiet.
		fmt.Fprintf(os.Stderr, "To sign in, visit: %s\n", da.VerificationURI)
		fmt.Fprintf(os.Stderr, "Enter code: %s\n", da.UserCode)
	}, logger)
	if err != nil {
		return err
	}

	logger.Info("login successful", "drive", flagDrive)
	statusf("Login successful.\n")

	return nil
}

func runLogout(_ *cobra.Command, _ []string) error {
	logger := buildLogger()

	tokenPath, err := authTokenPath()
	if err != nil {
		return err
	}

	logger.Info("logout started", "drive", flagDrive, "token_path", tokenPath)

	if err := graph.Logout(tokenPath, logger); err != nil {
		return err
	}

	logger.Info("logout successful", "drive", flagDrive)
	statusf("Logged out.\n")

	return nil
}

// whoamiOutput is the JSON schema for `whoami --json`.
type whoamiOutput struct {
	User   whoamiUser    `json:"user"`
	Drives []whoamiDrive `json:"drives"`
}

type whoamiUser struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
}

type whoamiDrive struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	DriveType  string `json:"drive_type"`
	QuotaUsed  int64  `json:"quota_used"`
	QuotaTotal int64  `json:"quota_total"`
}

func runWhoami(_ *cobra.Command, _ []string) error {
	logger := buildLogger()
	ctx := context.Background()

	tokenPath, err := authTokenPath()
	if err != nil {
		return err
	}

	logger.Debug("whoami", "drive", flagDrive, "token_path", tokenPath)

	ts, err := graph.TokenSourceFromPath(ctx, tokenPath, logger)
	if err != nil {
		if errors.Is(err, graph.ErrNotLoggedIn) {
			return fmt.Errorf("not logged in — run 'onedrive-go login --drive %s' first", flagDrive)
		}

		return err
	}

	client := graph.NewClient(graph.DefaultBaseURL, http.DefaultClient, ts, logger)

	user, err := client.Me(ctx)
	if err != nil {
		return fmt.Errorf("fetching user profile: %w", err)
	}

	drives, err := client.Drives(ctx)
	if err != nil {
		return fmt.Errorf("listing drives: %w", err)
	}

	if flagJSON {
		return printWhoamiJSON(user, drives)
	}

	printWhoamiText(user, drives)

	return nil
}

func printWhoamiJSON(user *graph.User, drives []graph.Drive) error {
	out := whoamiOutput{
		User: whoamiUser{
			ID:          user.ID,
			DisplayName: user.DisplayName,
			Email:       user.Email,
		},
		Drives: make([]whoamiDrive, 0, len(drives)),
	}

	for _, d := range drives {
		out.Drives = append(out.Drives, whoamiDrive{
			ID:         d.ID,
			Name:       d.Name,
			DriveType:  d.DriveType,
			QuotaUsed:  d.QuotaUsed,
			QuotaTotal: d.QuotaTotal,
		})
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

func printWhoamiText(user *graph.User, drives []graph.Drive) {
	fmt.Printf("User:  %s (%s)\n", user.DisplayName, user.Email)
	fmt.Printf("ID:    %s\n", user.ID)

	for _, d := range drives {
		fmt.Printf("\nDrive: %s (%s)\n", d.Name, d.DriveType)
		fmt.Printf("  ID:    %s\n", d.ID)
		fmt.Printf("  Quota: %s / %s\n", formatSize(d.QuotaUsed), formatSize(d.QuotaTotal))
	}
}
