//go:build integration

package graph

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	integrationTimeout = 30 * time.Second
	defaultTestProfile = "personal"
	profileEnvVar      = "ONEDRIVE_TEST_PROFILE"
	driveIDEnvVar      = "ONEDRIVE_TEST_DRIVE_ID"
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
		t.Skipf("no token for profile %q -- run bootstrap first", profile)
	}
	require.NoError(t, err, "loading token for profile %q", profile)

	return NewClient(DefaultBaseURL, http.DefaultClient, ts, logger)
}

// driveIDForTest reads the test drive ID from ONEDRIVE_TEST_DRIVE_ID.
// Skips the test if not set. Populated by bootstrap tool (--print-drive-id)
// or CI workflow.
func driveIDForTest(t *testing.T) string {
	t.Helper()

	id := os.Getenv(driveIDEnvVar)
	if id == "" {
		t.Skipf("%s not set -- run: go run ./cmd/integration-bootstrap --print-drive-id", driveIDEnvVar)
	}

	return id
}

// TestIntegration_GetItem verifies GetItem returns a properly normalized Item
// for the drive root.
func TestIntegration_GetItem(t *testing.T) {
	client := newIntegrationClient(t)
	driveID := driveIDForTest(t)

	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()

	item, err := client.GetItem(ctx, driveID, "root")
	require.NoError(t, err)

	assert.NotEmpty(t, item.ID, "root item ID should be non-empty")
	assert.NotEmpty(t, item.Name, "root item name should be non-empty")
	assert.True(t, item.IsFolder, "root should be a folder")
	// DriveID normalization: must be lowercase regardless of what the API returns.
	assert.Equal(t, strings.ToLower(item.DriveID), item.DriveID, "DriveID should be lowercase")

	t.Logf("root item: id=%s name=%s driveID=%s isFolder=%v", item.ID, item.Name, item.DriveID, item.IsFolder)
}

// TestIntegration_ListChildren verifies ListChildren returns items for the drive root.
func TestIntegration_ListChildren(t *testing.T) {
	client := newIntegrationClient(t)
	driveID := driveIDForTest(t)

	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()

	items, err := client.ListChildren(ctx, driveID, "root")
	require.NoError(t, err)

	// Most OneDrive accounts have at least one item in root.
	assert.NotEmpty(t, items, "root children should not be empty")

	for i, item := range items {
		assert.NotEmpty(t, item.ID, "item %d should have an ID", i)
		assert.NotEmpty(t, item.Name, "item %d should have a name", i)

		t.Logf("child[%d]: id=%s name=%s isFolder=%v", i, item.ID, item.Name, item.IsFolder)
	}
}

// TestIntegration_GetItem_NotFound verifies that requesting a nonexistent item
// returns ErrNotFound or ErrBadRequest (Graph API returns 400 for invalid ID formats).
func TestIntegration_GetItem_NotFound(t *testing.T) {
	client := newIntegrationClient(t)
	driveID := driveIDForTest(t)

	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()

	_, err := client.GetItem(ctx, driveID, "nonexistent-item-id-that-does-not-exist")
	require.Error(t, err)

	// Graph API returns 400 for invalid ID format, 404 for valid format but missing.
	// Accept either (see LEARNINGS.md section 5).
	isExpectedErr := errors.Is(err, ErrNotFound) || errors.Is(err, ErrBadRequest)
	assert.True(t, isExpectedErr, "expected ErrNotFound or ErrBadRequest, got: %v", err)
}

// TestIntegration_CreateAndDeleteFolder creates a test folder, verifies it,
// then deletes it and confirms deletion.
func TestIntegration_CreateAndDeleteFolder(t *testing.T) {
	client := newIntegrationClient(t)
	driveID := driveIDForTest(t)

	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()

	folderName := fmt.Sprintf("onedrive-go-test-%d", time.Now().UnixNano())

	// Register cleanup first to handle test failures.
	var createdID string

	t.Cleanup(func() {
		if createdID == "" {
			return
		}

		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), integrationTimeout)
		defer cleanCancel()

		// Best-effort cleanup -- don't fail the test if cleanup fails.
		_ = client.DeleteItem(cleanCtx, driveID, createdID)

		t.Logf("cleanup: deleted test folder %s", createdID)
	})

	// Create folder in root.
	folder, err := client.CreateFolder(ctx, driveID, "root", folderName)
	require.NoError(t, err)

	createdID = folder.ID

	assert.NotEmpty(t, folder.ID)
	assert.Equal(t, folderName, folder.Name)
	assert.True(t, folder.IsFolder, "created item should be a folder")

	t.Logf("created test folder: id=%s name=%s", folder.ID, folder.Name)

	// Delete the folder.
	err = client.DeleteItem(ctx, driveID, folder.ID)
	require.NoError(t, err)

	createdID = "" // Prevent double-delete in cleanup.

	t.Logf("deleted test folder: id=%s", folder.ID)

	// Verify it's gone. Graph API may return 404 or 400 for deleted items.
	_, err = client.GetItem(ctx, driveID, folder.ID)
	require.Error(t, err, "GetItem should fail after deletion")
}
