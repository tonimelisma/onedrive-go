//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/retry"
	"github.com/tonimelisma/onedrive-go/testutil"
)

const authPreflightTimeout = 30 * time.Second

func TestE2E_AuthPreflight_Fast(t *testing.T) {
	registerLogDump(t)

	for _, driveID := range liveConfig.CandidateDriveIDs() {
		driveID := driveID
		t.Run(sanitizeTestName(driveID), func(t *testing.T) {
			assertLiveAuthPreflight(t, driveID)
		})
	}
}

func assertLiveAuthPreflight(t *testing.T, driveID string) {
	t.Helper()

	tokenPath := filepath.Join(testDataDir, testutil.TokenFileName(driveID))
	ts, err := graph.TokenSourceFromPath(t.Context(), tokenPath, slog.New(slog.DiscardHandler))
	require.NoError(t, err, "load token source for %s", driveID)

	httpClient := &http.Client{}

	started := time.Now()
	meAttempts, err := waitForAuthPreflightEndpoint(t.Context(), httpClient, ts, "/me")
	require.NoErrorf(t, err, "%s", formatAuthPreflightFailure(driveID, "/me", time.Since(started), meAttempts))

	started = time.Now()
	drivesAttempts, err := waitForAuthPreflightEndpoint(t.Context(), httpClient, ts, "/me/drives")
	require.NoErrorf(t, err, "%s", formatAuthPreflightFailure(driveID, "/me/drives", time.Since(started), drivesAttempts))
}

func waitForAuthPreflightEndpoint(
	parent context.Context,
	httpClient *http.Client,
	ts graph.TokenSource,
	path string,
) ([]authPreflightAttempt, error) {
	ctx, cancel := context.WithTimeout(parent, authPreflightTimeout)
	defer cancel()

	var attempts []authPreflightAttempt

	for attempt := 0; ; attempt++ {
		result := runAuthPreflightAttempt(ctx, httpClient, ts, path)
		attempts = append(attempts, result)
		if result.succeeded() {
			return attempts, nil
		}

		decision := classifyAuthPreflightAttempt(path, result)
		if !decision.Retry {
			return attempts, fmt.Errorf("auth preflight endpoint %s returned a non-retryable failure: %s", path, decision.Reason)
		}

		if err := retry.TimeSleep(ctx, pollBackoff(attempt)); err != nil {
			return attempts, err
		}
	}
}

func runAuthPreflightAttempt(
	ctx context.Context,
	httpClient *http.Client,
	ts graph.TokenSource,
	path string,
) authPreflightAttempt {
	token, err := ts.Token()
	if err != nil {
		return authPreflightAttempt{Err: fmt.Sprintf("load token: %v", err)}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, graph.DefaultBaseURL+path, nil)
	if err != nil {
		return authPreflightAttempt{Err: fmt.Sprintf("build request: %v", err)}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "onedrive-go/e2e-preflight")

	resp, err := httpClient.Do(req)
	if err != nil {
		return authPreflightAttempt{Err: fmt.Sprintf("dispatch request: %v", err)}
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if readErr != nil {
		return authPreflightAttempt{
			StatusCode: resp.StatusCode,
			RequestID:  resp.Header.Get("request-id"),
			Err:        fmt.Sprintf("read response body: %v", readErr),
		}
	}

	result := authPreflightAttempt{
		StatusCode: resp.StatusCode,
		Code:       authPreflightGraphCode(body),
		RequestID:  resp.Header.Get("request-id"),
	}
	if resp.StatusCode != http.StatusOK {
		result.Err = strings.TrimSpace(string(body))
	}

	return result
}

func authPreflightGraphCode(body []byte) string {
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}

	return payload.Error.Code
}
