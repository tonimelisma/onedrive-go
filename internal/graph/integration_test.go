//go:build integration

package graph

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	graphBaseURL        = "https://graph.microsoft.com/v1.0"
	integrationTimeout  = 30 * time.Second
	defaultTestProfile  = "personal"
	profileEnvVar       = "ONEDRIVE_TEST_PROFILE"
)

// testLogger returns an slog.Logger at Debug level that writes to t.Log,
// so all token and request activity appears in CI output with -v.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()

	return slog.New(slog.NewTextHandler(testLogWriter{t: t}, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
}

// testLogWriter adapts testing.T.Log to io.Writer for slog output.
type testLogWriter struct {
	t *testing.T
}

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}

// newIntegrationClient loads a token for the test profile and returns
// a configured Client. Skips the test if no token is available.
func newIntegrationClient(t *testing.T) *Client {
	t.Helper()

	profile := os.Getenv(profileEnvVar)
	if profile == "" {
		profile = defaultTestProfile
	}

	ctx := context.Background()
	logger := testLogger(t)

	logger.Info("loading token for integration test",
		slog.String("profile", profile),
	)

	ts, err := TokenSourceFromProfile(ctx, profile, logger)
	if errors.Is(err, ErrNotLoggedIn) {
		t.Skipf("no token for profile %q — run bootstrap first", profile)
	}
	require.NoError(t, err, "loading token for profile %q", profile)

	return NewClient(graphBaseURL, http.DefaultClient, ts, logger)
}

func TestIntegration_GetMe(t *testing.T) {
	client := newIntegrationClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()

	resp, err := client.Do(ctx, http.MethodGet, "/me", nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(body, &result))

	assert.NotEmpty(t, result["displayName"], "displayName should be non-empty")
	assert.NotEmpty(t, result["id"], "id should be non-empty")

	t.Logf("authenticated as: %s", result["displayName"])
}

func TestIntegration_GetDriveRoot(t *testing.T) {
	client := newIntegrationClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()

	resp, err := client.Do(ctx, http.MethodGet, "/me/drive/root", nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(body, &result))

	assert.NotEmpty(t, result["id"], "root item id should be non-empty")
	assert.NotEmpty(t, result["name"], "root item name should be non-empty")

	t.Logf("drive root: id=%s name=%s", result["id"], result["name"])
}

func TestIntegration_ListRootChildren(t *testing.T) {
	client := newIntegrationClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()

	resp, err := client.Do(ctx, http.MethodGet, "/me/drive/root/children", nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(body, &result))

	value, ok := result["value"]
	require.True(t, ok, "response should contain 'value' array")

	items, ok := value.([]interface{})
	require.True(t, ok, "value should be an array")

	t.Logf("root children count: %d", len(items))
}

func TestIntegration_InvalidPath_404(t *testing.T) {
	client := newIntegrationClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()

	// Path-based lookup returns 404; item-ID-based with invalid format returns 400.
	_, err := client.Do(ctx, http.MethodGet, "/me/drive/root:/nonexistent-path-that-does-not-exist", nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound), "expected ErrNotFound, got: %v", err)
}
