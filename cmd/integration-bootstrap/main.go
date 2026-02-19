// Thin wrapper around graph.Login for token bootstrapping.
// Replaced by cmd/onedrive-go login in increment 1.7.
//
// Usage: go run ./cmd/integration-bootstrap --profile personal
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func main() {
	profile := flag.String("profile", "personal", "profile name for token storage")
	flag.Parse()

	ctx := context.Background()
	logger := slog.Default()

	_, err := graph.Login(ctx, *profile, func(da graph.DeviceAuth) {
		fmt.Printf("Go to %s and enter code: %s\n", da.VerificationURI, da.UserCode)
	}, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "login failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Login successful. Token saved.")
}
