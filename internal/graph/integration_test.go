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

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/retry"
	"github.com/tonimelisma/onedrive-go/testutil"
)

const (
	// Live Graph calls occasionally sit behind real service latency on GitHub
	// runners. April 5, 2026 CI evidence showed a normal GET /me call stall for
	// roughly 60 seconds before the platform eventually recovered, so keep the
	// per-test budget above that observed tail latency instead of misclassifying
	// the runner/network jitter as a product regression.
	integrationTimeout = 90 * time.Second

	// Integration discovery keeps polling longer than the product policy so live
	// CI proves the Graph surface can recover without making CLI discovery wait
	// this long for ordinary users.
	integrationDriveDiscoveryAttempts = 10
	integrationDriveDiscoveryMaxDelay = 10 * time.Second

	driveEnvVar = "ONEDRIVE_TEST_DRIVE"
)

func TestMain(m *testing.M) {
	// Fallback to "../.." — internal/graph/ is two levels below module root.
	root := testutil.FindModuleRoot("../..")
	if _, err := testutil.LoadLiveTestConfig(root); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
		os.Exit(1)
	}
	testutil.ValidateAllowlist(driveEnvVar)

	cleanup := setupIntegrationIsolation()

	// Run tests, then clean up explicitly. os.Exit does not run defers,
	// so we must call cleanup before exiting to preserve rotated tokens.
	code := m.Run()
	cleanup()
	os.Exit(code)
}

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

// newIntegrationClient loads a token for the test drive and returns
// a configured Client. Skips the test if no token is available.
func newIntegrationClient(t *testing.T) *Client {
	t.Helper()

	drive := os.Getenv(driveEnvVar)
	require.NotEmpty(t, drive, "ONEDRIVE_TEST_DRIVE not set")

	ctx := t.Context()
	logger := testLogger(t)

	cid := driveid.MustCanonicalID(drive)

	tokenPath := config.DriveTokenPath(cid)
	require.NotEmpty(t, tokenPath, "cannot determine token path for drive %q", drive)

	logger.Info("loading token for integration test",
		slog.String("drive", drive),
		slog.String("token_path", tokenPath),
	)

	ts, err := TokenSourceFromPath(ctx, tokenPath, logger)
	if errors.Is(err, ErrNotLoggedIn) {
		t.Skipf("no token for drive %q at %s -- run bootstrap first", drive, tokenPath)
	}
	require.NoError(t, err, "loading token for drive %q", drive)

	httpClient := retryHTTPClient(http.DefaultClient, retry.TransportPolicy())

	client := MustNewClient(DefaultBaseURL, httpClient, ts, logger, "onedrive-go/test")
	client.driveDiscoveryPolicy = integrationDriveDiscoveryPolicy()
	return client
}

func integrationDriveDiscoveryPolicy() retry.Policy {
	return retry.Policy{
		MaxAttempts: integrationDriveDiscoveryAttempts,
		Base:        time.Second,
		Max:         integrationDriveDiscoveryMaxDelay,
		Multiplier:  2,
		Jitter:      0.25,
	}
}

// driveIDForTest reads drive_id from the managed catalog entry for the test
// drive. The catalog is in the isolated data dir (copied from .testdata/ by
// setupIntegrationIsolation). No extra env var is needed.
func driveIDForTest(t *testing.T) driveid.ID {
	t.Helper()

	drive := os.Getenv(driveEnvVar)
	require.NotEmpty(t, drive, "ONEDRIVE_TEST_DRIVE not set")

	cid := driveid.MustCanonicalID(drive)

	catalog, err := config.LoadCatalog()
	require.NoError(t, err, "reading catalog for drive %q", drive)
	driveRecord, found := catalog.DriveByCanonicalID(cid)
	require.True(t, found, "catalog drive record missing — re-run scripts/bootstrap-test-credentials.sh")
	require.NotEmpty(t, driveRecord.RemoteDriveID, "catalog drive record has empty remote_drive_id")

	return driveid.New(driveRecord.RemoteDriveID)
}

// TestIntegration_GetItem verifies GetItem returns a properly normalized Item
// for the drive root.
func TestIntegration_GetItem(t *testing.T) {
	client := newIntegrationClient(t)
	driveID := driveIDForTest(t)

	ctx, cancel := context.WithTimeout(t.Context(), integrationTimeout)
	defer cancel()

	item, err := client.GetItem(ctx, driveID, "root")
	require.NoError(t, err)

	assert.NotEmpty(t, item.ID, "root item ID should be non-empty")
	assert.NotEmpty(t, item.Name, "root item name should be non-empty")
	assert.True(t, item.IsFolder, "root should be a folder")
	// DriveID normalization: must be lowercase regardless of what the API returns.
	assert.Equal(t, strings.ToLower(item.DriveID.String()), item.DriveID.String(), "DriveID should be lowercase")

	t.Logf("root item: id=%s name=%s driveID=%s isFolder=%v", item.ID, item.Name, item.DriveID.String(), item.IsFolder)
}

// TestIntegration_ListChildren verifies ListChildren returns items for the drive root.
func TestIntegration_ListChildren(t *testing.T) {
	client := newIntegrationClient(t)
	driveID := driveIDForTest(t)

	ctx, cancel := context.WithTimeout(t.Context(), integrationTimeout)
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

	ctx, cancel := context.WithTimeout(t.Context(), integrationTimeout)
	defer cancel()

	_, err := client.GetItem(ctx, driveID, "nonexistent-item-id-that-does-not-exist")
	require.Error(t, err)

	// Graph API returns 400 for invalid ID format, 404 for valid format but missing.
	// Accept either (see spec/reference/graph-api-quirks.md).
	isExpectedErr := errors.Is(err, ErrNotFound) || errors.Is(err, ErrBadRequest)
	assert.True(t, isExpectedErr, "expected ErrNotFound or ErrBadRequest, got: %v", err)
}

// TestIntegration_Me verifies Me() returns a valid user profile.
func TestIntegration_Me(t *testing.T) {
	client := newIntegrationClient(t)

	ctx, cancel := context.WithTimeout(t.Context(), integrationTimeout)
	defer cancel()

	user, err := client.Me(ctx)
	require.NoError(t, err)

	assert.NotEmpty(t, user.DisplayName, "DisplayName should be non-empty")
	assert.NotEmpty(t, user.ID, "ID should be non-empty")

	t.Logf("user: id=%s displayName=%s email=%s", user.ID, user.DisplayName, user.Email)
}

// TestIntegration_Drives verifies Drives() returns at least one drive.
func TestIntegration_Drives(t *testing.T) {
	client := newIntegrationClient(t)

	ctx, cancel := context.WithTimeout(t.Context(), integrationTimeout)
	defer cancel()

	drives, err := client.Drives(ctx)
	require.NoError(t, err)

	assert.NotEmpty(t, drives, "should have at least one drive")

	for i, d := range drives {
		assert.NotEmpty(t, d.ID, "drive %d should have an ID", i)
		assert.NotEmpty(t, d.DriveType, "drive %d should have a drive type", i)

		t.Logf("drive[%d]: id=%s name=%s type=%s", i, d.ID, d.Name, d.DriveType)
	}
}

// Validates: R-6.7.11
func TestIntegration_Drives_PersonalAccountFiltersPhantomDrives(t *testing.T) {
	if !strings.HasPrefix(os.Getenv(driveEnvVar), "personal:") {
		t.Skip("skipping: test requires a Personal account")
	}

	client := newIntegrationClient(t)

	ctx, cancel := context.WithTimeout(t.Context(), integrationTimeout)
	defer cancel()

	drives, err := client.Drives(ctx)
	require.NoError(t, err)

	personalCount := 0
	for _, drive := range drives {
		if drive.DriveType == driveid.DriveTypePersonal {
			personalCount++
		}
	}

	assert.Equal(t, 1, personalCount, "personal drive discovery should return exactly one authoritative personal drive")
}

// TestIntegration_SearchDriveItems verifies shared discovery search returns without error.
// The result may be empty if Graph does not currently surface shared items for the test account.
func TestIntegration_SearchDriveItems(t *testing.T) {
	client := newIntegrationClient(t)

	ctx, cancel := context.WithTimeout(t.Context(), integrationTimeout)
	defer cancel()

	items, err := client.SearchDriveItems(ctx, "*")
	require.NoError(t, err)

	for _, item := range items {
		if (item.IsFolder || item.RemoteIsFolder) && item.RemoteDriveID != "" {
			t.Logf("shared folder: %s (owner=%s, remoteDrive=%s, remoteItem=%s)",
				item.Name, item.SharedOwnerEmail, item.RemoteDriveID, item.RemoteItemID)
		}
	}

	t.Logf("SearchDriveItems returned %d items", len(items))
}

// TestIntegration_CreateAndDeleteFolder creates a test folder, verifies it,
// then deletes it and confirms deletion.
func TestIntegration_CreateAndDeleteFolder(t *testing.T) {
	client := newIntegrationClient(t)
	driveID := driveIDForTest(t)

	ctx, cancel := context.WithTimeout(t.Context(), integrationTimeout)
	defer cancel()

	folderName := fmt.Sprintf("onedrive-go-test-%d", time.Now().UnixNano())

	// Register cleanup first to handle test failures.
	var createdID string

	t.Cleanup(func() {
		if createdID == "" {
			return
		}

		cleanCtx, cleanCancel := context.WithTimeout(t.Context(), integrationTimeout)
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
