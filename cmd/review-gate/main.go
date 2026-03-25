package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/reviewgate"
)

const (
	commandTimeout = 30 * time.Second
	httpTimeout    = 15 * time.Second
)

func main() {
	os.Exit(run(context.Background(), os.Stdout, os.Stderr))
}

func run(ctx context.Context, stdout *os.File, stderr *os.File) int {
	exitCode, err := runMain(ctx, stdout)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "review-gate: %v\n", err)
		return 1
	}

	return exitCode
}

func runMain(ctx context.Context, stdout *os.File) (int, error) {
	eventPath := os.Getenv("GITHUB_EVENT_PATH")
	if eventPath == "" {
		return 0, fmt.Errorf("missing GITHUB_EVENT_PATH")
	}

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return 0, fmt.Errorf("missing GITHUB_TOKEN")
	}

	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	pullRequest, err := reviewgate.LoadPullRequest(eventPath, os.Getenv("GITHUB_REPOSITORY"))
	if err != nil {
		return 0, fmt.Errorf("load pull request from event: %w", err)
	}

	gate := reviewgate.NewGate(
		reviewgate.NewClient(
			&http.Client{Timeout: httpTimeout},
			os.Getenv("GITHUB_API_URL"),
			token,
		),
		reviewgate.Config{
			CodexReviewLogin: os.Getenv("CODEX_REVIEW_LOGIN"),
		},
	)

	decision, err := gate.Evaluate(ctx, pullRequest)
	if err != nil {
		return 0, fmt.Errorf("evaluate review gate: %w", err)
	}

	if _, err := fmt.Fprintln(stdout, decision.Message); err != nil {
		return 0, fmt.Errorf("write decision: %w", err)
	}

	if decision.Status == reviewgate.DecisionFail {
		return 1, nil
	}

	return 0, nil
}
