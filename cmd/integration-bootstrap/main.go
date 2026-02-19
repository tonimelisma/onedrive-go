// Thin wrapper around graph.Login for token bootstrapping.
// Replaced by cmd/onedrive-go login in increment 1.7.
//
// Usage:
//
//	go run ./cmd/integration-bootstrap --profile personal
//	go run ./cmd/integration-bootstrap --print-drive-id --profile personal
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func main() {
	profile := flag.String("profile", "personal", "profile name for token storage")
	printDriveID := flag.Bool("print-drive-id", false, "print the drive ID for the profile (requires existing token)")
	flag.Parse()

	ctx := context.Background()
	logger := slog.Default()

	if *printDriveID {
		printDrive(ctx, *profile, logger)
		return
	}

	_, err := graph.Login(ctx, *profile, func(da graph.DeviceAuth) {
		fmt.Printf("Go to %s and enter code: %s\n", da.VerificationURI, da.UserCode)
	}, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "login failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Login successful. Token saved.")
}

// printDrive loads an existing token and fetches the user's default drive ID.
// Prints only the drive ID to stdout for use in shell scripting:
//
//	export ONEDRIVE_TEST_DRIVE_ID=$(go run ./cmd/integration-bootstrap --print-drive-id)
func printDrive(ctx context.Context, profile string, logger *slog.Logger) {
	ts, err := graph.TokenSourceFromProfile(ctx, profile, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load token for profile %q: %v\n", profile, err)
		os.Exit(1)
	}

	client := graph.NewClient(graph.DefaultBaseURL, http.DefaultClient, ts, logger)

	resp, err := client.Do(ctx, http.MethodGet, "/me/drive", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to fetch drive: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read response: %v\n", err)
		os.Exit(1)
	}

	var result struct {
		ID string `json:"id"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		fmt.Fprintf(os.Stderr, "failed to parse drive response: %v\n", err)
		os.Exit(1)
	}

	if result.ID == "" {
		fmt.Fprintln(os.Stderr, "drive ID is empty in response")
		os.Exit(1)
	}

	// Print only the ID, no newline decoration, for shell capture.
	fmt.Print(result.ID)
}
