//go:build integration

package sync

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

const (
	integrationTimeout      = 60 * time.Second
	integrationDriveEnvVar  = "ONEDRIVE_TEST_DRIVE"
	integrationDriveIDEnv   = "ONEDRIVE_TEST_DRIVE_ID"
	integrationDefaultDrive = "personal:test@example.com"
)

// integrationLogger returns a debug-level slog.Logger writing to t.Log.
func integrationLogger(t *testing.T) *slog.Logger {
	t.Helper()

	return slog.New(slog.NewTextHandler(integrationLogWriter{t: t}, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
}

// integrationLogWriter adapts testing.T to io.Writer.
type integrationLogWriter struct{ t *testing.T }

func (w integrationLogWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}

// newIntegrationEngine creates an Engine backed by a real Graph client and
// a file-backed SQLiteStore. The sync directory is an empty temp dir so
// no local files are affected. Returns the engine and a cleanup function.
func newIntegrationEngine(t *testing.T) *Engine {
	t.Helper()

	drive := os.Getenv(integrationDriveEnvVar)
	if drive == "" {
		drive = integrationDefaultDrive
	}

	driveID := os.Getenv(integrationDriveIDEnv)
	if driveID == "" {
		t.Skip("ONEDRIVE_TEST_DRIVE_ID not set — skip sync integration tests")
	}

	logger := integrationLogger(t)
	ctx := context.Background()

	tokenPath := config.DriveTokenPath(drive)
	if tokenPath == "" {
		t.Fatalf("cannot determine token path for drive %q", drive)
	}

	ts, err := graph.TokenSourceFromPath(ctx, tokenPath, logger)
	if err != nil {
		t.Skipf("no token for drive %q: %v — run bootstrap first", drive, err)
	}

	client := graph.NewClient(graph.DefaultBaseURL, http.DefaultClient, ts, logger)

	// Use a temp directory for both sync root and state DB.
	tmpDir := t.TempDir()
	syncDir := filepath.Join(tmpDir, "sync")
	require.NoError(t, os.MkdirAll(syncDir, 0o755))

	dbPath := filepath.Join(tmpDir, "state.db")

	store, err := NewStore(dbPath, logger)
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})

	resolved := &config.ResolvedDrive{
		CanonicalID: drive,
		DriveID:     driveID,
		SyncDir:     syncDir,
		FilterConfig: config.FilterConfig{
			MaxFileSize:  "50GB",
			IgnoreMarker: ".odignore",
		},
		SafetyConfig: config.SafetyConfig{
			BigDeleteThreshold:     100,
			BigDeletePercentage:    50,
			BigDeleteMinItems:      10,
			MinFreeSpace:           "1GB",
			TombstoneRetentionDays: 30,
		},
		TransfersConfig: config.TransfersConfig{},
	}

	eng, err := NewEngine(store, client, resolved, logger)
	require.NoError(t, err)

	t.Cleanup(func() { eng.Close() })

	return eng
}

// TestIntegration_NewEngine verifies that engine construction succeeds with
// a real graph client — all components wire together without errors.
func TestIntegration_NewEngine(t *testing.T) {
	eng := newIntegrationEngine(t)
	assert.NotNil(t, eng)
}

// TestIntegration_RunOnce_DryRun runs a full sync cycle in dry-run mode against
// the real API. This exercises delta fetch (real API), local scan (empty dir),
// reconciliation, and safety checking without executing any writes.
// Since remote_path scoping isn't implemented yet, this fetches the entire drive's
// delta — but dry-run ensures no changes are made.
func TestIntegration_RunOnce_DryRun(t *testing.T) {
	eng := newIntegrationEngine(t)

	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()

	report, err := eng.RunOnce(ctx, SyncBidirectional, SyncOptions{DryRun: true})
	require.NoError(t, err)

	// Report should have timing metadata.
	assert.True(t, report.StartedAt > 0, "StartedAt should be set")
	assert.True(t, report.CompletedAt >= report.StartedAt, "CompletedAt >= StartedAt")
	assert.True(t, report.DryRun, "should report as dry run")
	assert.Equal(t, SyncBidirectional, report.Mode)

	t.Logf("dry-run report: downloads=%d uploads=%d deletes=%d conflicts=%d",
		report.Downloaded, report.Uploaded,
		report.LocalDeleted+report.RemoteDeleted, report.Conflicts)
}

// TestIntegration_RunOnce_DownloadOnly_DryRun runs download-only mode in dry-run
// to verify delta fetching works against the real API.
func TestIntegration_RunOnce_DownloadOnly_DryRun(t *testing.T) {
	eng := newIntegrationEngine(t)

	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()

	report, err := eng.RunOnce(ctx, SyncDownloadOnly, SyncOptions{DryRun: true})
	require.NoError(t, err)

	assert.Equal(t, SyncDownloadOnly, report.Mode)
	assert.True(t, report.DryRun)
}
