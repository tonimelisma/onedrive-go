package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// --- driveReportsError ---

func TestDriveReportsError(t *testing.T) {
	t.Parallel()

	errDelta := fmt.Errorf("delta expired")
	errAuth := fmt.Errorf("auth failed")

	tests := []struct {
		name    string
		reports []*synctypes.DriveReport
		wantNil bool
		wantMsg string // substring, only checked when wantNil is false
	}{
		{
			name:    "zero reports",
			reports: nil,
			wantNil: true,
		},
		{
			name: "one success",
			reports: []*synctypes.DriveReport{
				{Report: &synctypes.SyncReport{Mode: synctypes.SyncBidirectional}},
			},
			wantNil: true,
		},
		{
			name: "one failure",
			reports: []*synctypes.DriveReport{
				{Err: errDelta},
			},
			wantMsg: "delta expired",
		},
		{
			name: "multi-drive mixed",
			reports: []*synctypes.DriveReport{
				{Report: &synctypes.SyncReport{}},
				{Err: errDelta},
			},
			wantMsg: "1 of 2 drives failed",
		},
		{
			name: "all failures",
			reports: []*synctypes.DriveReport{
				{Err: errDelta},
				{Err: errAuth},
			},
			wantMsg: "2 of 2 drives failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := driveReportsError(tt.reports)
			if tt.wantNil {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantMsg)
			}
		})
	}
}

// --- printDriveReports ---

// quietCC returns a CLIContext with Quiet=true for tests that call status-printing
// functions. Only populates the Flags field — other fields are nil/zero.
func quietCC() *CLIContext {
	return &CLIContext{Flags: CLIFlags{Quiet: true}}
}

func newSyncTestContext(parent context.Context, cfgPath string, statusWriter io.Writer) context.Context {
	cc := &CLIContext{
		Logger:       slog.New(slog.DiscardHandler),
		OutputWriter: io.Discard,
		StatusWriter: statusWriter,
		CfgPath:      cfgPath,
	}

	return context.WithValue(parent, cliContextKey{}, cc)
}

func TestPrintDriveReports_SingleDrive_NoHeader(t *testing.T) {
	t.Parallel()

	reports := []*synctypes.DriveReport{
		{
			DisplayName: "Personal",
			Report:      &synctypes.SyncReport{Mode: synctypes.SyncBidirectional},
		},
	}

	// Should not panic or produce headers for single drive.
	printDriveReports(reports, quietCC())
}

func TestPrintDriveReports_MultiDrive_WithError(t *testing.T) {
	t.Parallel()

	reports := []*synctypes.DriveReport{
		{
			DisplayName: "Personal",
			Report:      &synctypes.SyncReport{Mode: synctypes.SyncBidirectional},
		},
		{
			DisplayName: "Business",
			Err:         fmt.Errorf("sync failed"),
		},
	}

	// Should not panic. Output goes to stderr via statusf.
	printDriveReports(reports, quietCC())
}

// --- syncModeFromFlags ---

// Validates: R-2.1
func TestSyncModeFromFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		flag string
		want synctypes.SyncMode
	}{
		{"default bidirectional", "", synctypes.SyncBidirectional},
		{"download-only", "download-only", synctypes.SyncDownloadOnly},
		{"upload-only", "upload-only", synctypes.SyncUploadOnly},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cmd := newSyncCmd()
			if tt.flag != "" {
				require.NoError(t, cmd.Flags().Set(tt.flag, "true"))
			}

			assert.Equal(t, tt.want, syncModeFromFlags(cmd))
		})
	}
}

// --- printNonZero ---

func TestPrintNonZero(t *testing.T) {
	t.Parallel()

	// n=0 should produce no output; n>0 should produce labeled line.
	cc := quietCC()
	printNonZero(cc, "Downloads", 0) // should not panic
	printNonZero(cc, "Downloads", 5) // should not panic
}

// --- printSyncReport ---

func TestPrintSyncReport(t *testing.T) {
	t.Parallel()

	cc := quietCC()

	// Dry-run report.
	printSyncReport(&synctypes.SyncReport{DryRun: true, Mode: synctypes.SyncBidirectional}, cc)

	// No-changes report.
	printSyncReport(&synctypes.SyncReport{Mode: synctypes.SyncUploadOnly}, cc)

	// Full report with results.
	printSyncReport(&synctypes.SyncReport{
		Downloads: 3,
		Uploads:   2,
		Succeeded: 5,
		Mode:      synctypes.SyncDownloadOnly,
	}, cc)
}

// --- newSyncCmd ---

func TestNewSyncCmd_Structure(t *testing.T) {
	t.Parallel()

	cmd := newSyncCmd()
	assert.Equal(t, "sync", cmd.Use)

	for _, flag := range []string{"download-only", "upload-only", "dry-run", "force", "watch", "full"} {
		assert.NotNil(t, cmd.Flags().Lookup(flag), "missing flag %q", flag)
	}
}

func TestNewSyncCmd_FullWatchMutualExclusivity(t *testing.T) {
	t.Parallel()

	cmd := newSyncCmd()
	require.NoError(t, cmd.Flags().Set("full", "true"))
	require.NoError(t, cmd.Flags().Set("watch", "true"))

	// Cobra validates mutual exclusivity during PreRunE / ValidateFlagGroups.
	err := cmd.ValidateFlagGroups()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "full")
	assert.Contains(t, err.Error(), "watch")
}

func TestParseDurationOrZero(t *testing.T) {
	t.Parallel()

	assert.Zero(t, parseDurationOrZero(""))
	assert.Zero(t, parseDurationOrZero("not-a-duration"))
	assert.Equal(t, 5*time.Minute, parseDurationOrZero("5m"))
}

func TestParsePollInterval(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 30*time.Second, parsePollInterval("30s"))
	assert.Zero(t, parsePollInterval("bogus"))
}

func TestRunSync_LoadConfigError(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`["personal:test@example.com"`), 0o600))

	parent, cancel := context.WithCancel(t.Context())
	defer cancel()

	cmd := newSyncCmd()
	cmd.SetContext(newSyncTestContext(parent, cfgPath, io.Discard))

	err := runSync(cmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "loading config")
}

func TestRunSync_NoDrivesConfigured(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "missing-config.toml")

	parent, cancel := context.WithCancel(t.Context())
	defer cancel()

	cmd := newSyncCmd()
	cmd.SetContext(newSyncTestContext(parent, cfgPath, io.Discard))

	err := runSync(cmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no drives configured")
}

func TestRunSync_AllDrivesPaused(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	syncDir := t.TempDir()
	configBody := fmt.Sprintf(`
["personal:test@example.com"]
sync_dir = %q
paused = true
`, syncDir)
	require.NoError(t, os.WriteFile(cfgPath, []byte(configBody), 0o600))

	parent, cancel := context.WithCancel(t.Context())
	defer cancel()

	cmd := newSyncCmd()
	cmd.SetContext(newSyncTestContext(parent, cfgPath, io.Discard))

	err := runSync(cmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "all drives are paused")
}

// Validates: R-6.6.4
func TestRunSync_LogFileOpenFailureWarnsToStatusWriter(t *testing.T) {
	setTestDriveHome(t)

	parentDir := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(parentDir, []byte("x"), 0o600))

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	configBody := fmt.Sprintf("log_file = %q\n", filepath.Join(parentDir, "sync.log"))
	require.NoError(t, os.WriteFile(cfgPath, []byte(configBody), 0o600))

	var statusBuf bytes.Buffer

	parent, cancel := context.WithCancel(t.Context())
	defer cancel()

	cmd := newSyncCmd()
	cmd.SetContext(newSyncTestContext(parent, cfgPath, &statusBuf))

	err := runSync(cmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no drives configured")
	assert.Contains(t, statusBuf.String(), "cannot open log file")
}
