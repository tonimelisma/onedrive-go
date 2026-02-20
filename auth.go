package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"

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

func runLogin(_ *cobra.Command, _ []string) error {
	logger := buildLogger()
	ctx := context.Background()

	logger.Info("login started", "profile", flagProfile)

	_, err := graph.Login(ctx, flagProfile, func(da graph.DeviceAuth) {
		// Device code prompts must always be visible — not suppressed by --quiet.
		fmt.Fprintf(os.Stderr, "To sign in, visit: %s\n", da.VerificationURI)
		fmt.Fprintf(os.Stderr, "Enter code: %s\n", da.UserCode)
	}, logger)
	if err != nil {
		return err
	}

	logger.Info("login successful", "profile", flagProfile)
	statusf("Login successful.\n")

	return nil
}

func runLogout(_ *cobra.Command, _ []string) error {
	logger := buildLogger()

	logger.Info("logout started", "profile", flagProfile)

	if err := graph.Logout(flagProfile, logger); err != nil {
		return err
	}

	logger.Info("logout successful", "profile", flagProfile)
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

	logger.Debug("whoami", "profile", flagProfile)

	ts, err := graph.TokenSourceFromProfile(ctx, flagProfile, logger)
	if err != nil {
		if errors.Is(err, graph.ErrNotLoggedIn) {
			return fmt.Errorf("not logged in — run 'onedrive-go login' first")
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
