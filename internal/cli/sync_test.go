package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// --- driveReportsError ---

func TestDriveReportsError(t *testing.T) {
	t.Parallel()

	errDelta := fmt.Errorf("delta expired")
	errAuth := fmt.Errorf("auth failed")

	tests := []struct {
		name    string
		reports []*multisync.DriveReport
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
			reports: []*multisync.DriveReport{
				{Report: &syncengine.Report{Mode: syncengine.SyncBidirectional}},
			},
			wantNil: true,
		},
		{
			name: "one failure",
			reports: []*multisync.DriveReport{
				{Err: errDelta},
			},
			wantMsg: "delta expired",
		},
		{
			name: "reset required includes guidance",
			reports: []*multisync.DriveReport{
				{
					CanonicalID: driveid.MustCanonicalID("personal:reset@example.com"),
					Err: &syncengine.StateStoreIncompatibleError{
						Reason: syncengine.StateStoreIncompatibleReasonIncompatibleSchema,
					},
				},
			},
			wantMsg: "drive reset-sync-state --drive 'personal:reset@example.com'",
		},
		{
			name: "multi-drive mixed",
			reports: []*multisync.DriveReport{
				{Report: &syncengine.Report{}},
				{Err: errDelta},
			},
			wantMsg: "1 of 2 drives failed",
		},
		{
			name: "all failures",
			reports: []*multisync.DriveReport{
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

func statusCC() (*CLIContext, *bytes.Buffer) {
	status := &bytes.Buffer{}
	return &CLIContext{
		StatusWriter: status,
	}, status
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

	reports := []*multisync.DriveReport{
		{
			DisplayName: "Personal",
			Report:      &syncengine.Report{Mode: syncengine.SyncBidirectional},
		},
	}

	// Should not panic or produce headers for single drive.
	printDriveReports(reports, quietCC())
}

func TestPrintDriveReports_MultiDrive_WithError(t *testing.T) {
	t.Parallel()

	reports := []*multisync.DriveReport{
		{
			DisplayName: "Personal",
			Report:      &syncengine.Report{Mode: syncengine.SyncBidirectional},
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
		want syncengine.SyncMode
	}{
		{"default bidirectional", "", syncengine.SyncBidirectional},
		{"download-only", "download-only", syncengine.SyncDownloadOnly},
		{"upload-only", "upload-only", syncengine.SyncUploadOnly},
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

	cc, status := statusCC()

	// Dry-run report.
	printSyncReport(&syncengine.Report{DryRun: true, Mode: syncengine.SyncBidirectional}, cc)

	// No-changes report.
	printSyncReport(&syncengine.Report{Mode: syncengine.SyncUploadOnly}, cc)

	// Full report with results.
	printSyncReport(&syncengine.Report{
		Downloads: 3,
		Uploads:   2,
		Succeeded: 5,
		Mode:      syncengine.SyncDownloadOnly,
	}, cc)

	output := status.String()
	assert.Contains(t, output, "Dry run")
	assert.Contains(t, output, "No changes detected")
	assert.Contains(t, output, "Plan:")
	assert.Contains(t, output, "Results:")
}

func TestPrintSyncReport_DeferredOnlyDoesNotRenderFalseIdleOrResults(t *testing.T) {
	t.Parallel()

	cc, status := statusCC()

	printSyncReport(&syncengine.Report{
		Mode: syncengine.SyncUploadOnly,
		DeferredByMode: syncengine.DeferredCounts{
			Downloads:    1,
			LocalDeletes: 1,
		},
	}, cc)

	output := status.String()
	assert.Contains(t, output, "Deferred by mode:")
	assert.Contains(t, output, "Downloads:")
	assert.Contains(t, output, "Local deletes:")
	assert.NotContains(t, output, "No changes detected")
	assert.NotContains(t, output, "Results:")
}

// --- newSyncCmd ---

func TestNewSyncCmd_Structure(t *testing.T) {
	t.Parallel()

	cmd := newSyncCmd()
	assert.Equal(t, "sync", cmd.Use)

	for _, flag := range []string{"download-only", "upload-only", "dry-run", "watch", "full"} {
		assert.NotNil(t, cmd.Flags().Lookup(flag), "missing flag %q", flag)
	}
	assert.Nil(t, cmd.Flags().Lookup("force"), "sync must not expose ad hoc force execution")
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

func TestSyncDryRunOverride_DefaultUnset(t *testing.T) {
	t.Parallel()

	cmd := newSyncCmd()

	override, set, err := syncDryRunOverride(cmd)
	require.NoError(t, err)
	assert.False(t, set)
	assert.False(t, override)
}

func TestSyncDryRunOverride_ExplicitTrue(t *testing.T) {
	t.Parallel()

	cmd := newSyncCmd()
	require.NoError(t, cmd.Flags().Set("dry-run", "true"))

	override, set, err := syncDryRunOverride(cmd)
	require.NoError(t, err)
	require.True(t, set)
	assert.True(t, override)
}

func TestSyncDryRunOverride_ExplicitFalse(t *testing.T) {
	t.Parallel()

	cmd := newSyncCmd()
	require.NoError(t, cmd.Flags().Set("dry-run", "false"))

	override, set, err := syncDryRunOverride(cmd)
	require.NoError(t, err)
	require.True(t, set)
	assert.False(t, override)
}

func TestResolveSyncDryRun(t *testing.T) {
	t.Parallel()

	cliTrue := true
	cliFalse := false

	tests := []struct {
		name       string
		cfgDryRun  bool
		override   *bool
		watch      bool
		wantDryRun bool
		wantErr    string
	}{
		{
			name:       "config default used when CLI flag absent",
			cfgDryRun:  true,
			wantDryRun: true,
		},
		{
			name:       "CLI true overrides config false",
			cfgDryRun:  false,
			override:   &cliTrue,
			wantDryRun: true,
		},
		{
			name:       "CLI false overrides config true",
			cfgDryRun:  true,
			override:   &cliFalse,
			wantDryRun: false,
		},
		{
			name:      "watch rejects effective dry run",
			cfgDryRun: true,
			watch:     true,
			wantErr:   "watch mode does not support dry-run",
		},
		{
			name:       "watch allows explicit CLI false override",
			cfgDryRun:  true,
			override:   &cliFalse,
			watch:      true,
			wantDryRun: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dryRun, err := resolveSyncDryRun(tt.cfgDryRun, tt.override, tt.watch)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantDryRun, dryRun)
		})
	}
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
	assert.Contains(t, err.Error(), "all selected drives are paused")
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

func TestRunSyncCommand_UsesConfigDryRunWhenFlagUnset(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	syncDir := t.TempDir()
	configBody := fmt.Sprintf(`
dry_run = true

["personal:test@example.com"]
sync_dir = %q
`, syncDir)
	require.NoError(t, os.WriteFile(cfgPath, []byte(configBody), 0o600))

	cc := &CLIContext{
		Logger:       slog.New(slog.DiscardHandler),
		OutputWriter: io.Discard,
		StatusWriter: io.Discard,
		CfgPath:      cfgPath,
	}
	called := false
	cc.syncRunOnceRunner = func(
		_ context.Context,
		_ *config.Holder,
		drives []*config.ResolvedDrive,
		mode syncengine.SyncMode,
		opts syncengine.RunOptions,
		_ *slog.Logger,
		controlSocketPath string,
	) multisync.RunOnceResult {
		called = true
		assert.Len(t, drives, 1)
		assert.Equal(t, syncengine.SyncBidirectional, mode)
		assert.True(t, opts.DryRun)
		assert.NotEmpty(t, controlSocketPath)

		return multisync.RunOnceResult{
			Startup: multisync.StartupSelectionSummary{
				Results: []multisync.DriveStartupResult{{
					CanonicalID: drives[0].CanonicalID,
					DisplayName: drives[0].DisplayName,
					Status:      multisync.DriveStartupRunnable,
				}},
			},
			Reports: []*multisync.DriveReport{
				{
					CanonicalID: drives[0].CanonicalID,
					DisplayName: drives[0].DisplayName,
					Report: &syncengine.Report{
						Mode:   mode,
						DryRun: opts.DryRun,
					},
				},
			},
		}
	}

	err := runSyncCommand(t.Context(), cc, syncCommandOptions{Mode: syncengine.SyncBidirectional})
	require.NoError(t, err)
	assert.True(t, called)
}

func TestRunSyncCommand_CreatesMissingSyncDirBeforeRunOnce(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	syncDir := filepath.Join(t.TempDir(), "missing", "sync-root")
	configBody := fmt.Sprintf(`
["personal:test@example.com"]
sync_dir = %q
`, syncDir)
	require.NoError(t, os.WriteFile(cfgPath, []byte(configBody), 0o600))

	cc := &CLIContext{
		Logger:       slog.New(slog.DiscardHandler),
		OutputWriter: io.Discard,
		StatusWriter: io.Discard,
		CfgPath:      cfgPath,
	}
	called := false
	cc.syncRunOnceRunner = func(
		_ context.Context,
		_ *config.Holder,
		drives []*config.ResolvedDrive,
		mode syncengine.SyncMode,
		_ syncengine.RunOptions,
		_ *slog.Logger,
		_ string,
	) multisync.RunOnceResult {
		called = true
		require.Len(t, drives, 1)
		assert.Equal(t, syncengine.SyncBidirectional, mode)

		info, err := os.Stat(syncDir)
		require.NoError(t, err)
		assert.True(t, info.IsDir())

		return multisync.RunOnceResult{
			Startup: multisync.StartupSelectionSummary{
				Results: []multisync.DriveStartupResult{{
					CanonicalID: drives[0].CanonicalID,
					DisplayName: drives[0].DisplayName,
					Status:      multisync.DriveStartupRunnable,
				}},
			},
			Reports: []*multisync.DriveReport{{
				CanonicalID: drives[0].CanonicalID,
				DisplayName: drives[0].DisplayName,
				Report: &syncengine.Report{
					Mode: mode,
				},
			}},
		}
	}

	err := runSyncCommand(t.Context(), cc, syncCommandOptions{Mode: syncengine.SyncBidirectional})
	require.NoError(t, err)
	assert.True(t, called)
}

func TestRunSyncCommand_CLIFalseOverridesConfigDryRun(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	syncDir := t.TempDir()
	configBody := fmt.Sprintf(`
dry_run = true

["personal:test@example.com"]
sync_dir = %q
`, syncDir)
	require.NoError(t, os.WriteFile(cfgPath, []byte(configBody), 0o600))

	cc := &CLIContext{
		Logger:       slog.New(slog.DiscardHandler),
		OutputWriter: io.Discard,
		StatusWriter: io.Discard,
		CfgPath:      cfgPath,
	}
	override := false
	cc.syncRunOnceRunner = func(
		_ context.Context,
		_ *config.Holder,
		_ []*config.ResolvedDrive,
		_ syncengine.SyncMode,
		opts syncengine.RunOptions,
		_ *slog.Logger,
		_ string,
	) multisync.RunOnceResult {
		assert.False(t, opts.DryRun)

		return multisync.RunOnceResult{
			Startup: multisync.StartupSelectionSummary{
				Results: []multisync.DriveStartupResult{{
					Status: multisync.DriveStartupRunnable,
				}},
			},
			Reports: []*multisync.DriveReport{{Report: &syncengine.Report{Mode: syncengine.SyncBidirectional}}},
		}
	}

	err := runSyncCommand(t.Context(), cc, syncCommandOptions{
		Mode:   syncengine.SyncBidirectional,
		DryRun: &override,
	})
	require.NoError(t, err)
}

func TestRunSyncCommand_WatchRejectsEffectiveDryRun(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	syncDir := t.TempDir()
	configBody := fmt.Sprintf(`
dry_run = true

["personal:test@example.com"]
sync_dir = %q
`, syncDir)
	require.NoError(t, os.WriteFile(cfgPath, []byte(configBody), 0o600))

	cc := &CLIContext{
		Logger:       slog.New(slog.DiscardHandler),
		OutputWriter: io.Discard,
		StatusWriter: io.Discard,
		CfgPath:      cfgPath,
		syncWatchRunner: func(
			context.Context,
			*config.Holder,
			[]string,
			syncengine.SyncMode,
			syncengine.WatchOptions,
			*slog.Logger,
			io.Writer,
			string,
		) error {
			require.FailNow(t, "watch runner should not be called when effective dry run is true")
			return nil
		},
	}

	err := runSyncCommand(t.Context(), cc, syncCommandOptions{
		Mode:  syncengine.SyncBidirectional,
		Watch: true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "watch mode does not support dry-run")
}

func TestRunSyncCommand_FailsLoudlyWhenControlSocketPathCannotBeDerivedForOneShot(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	syncDir := t.TempDir()
	configBody := fmt.Sprintf(`
["personal:test@example.com"]
sync_dir = %q
`, syncDir)
	require.NoError(t, os.WriteFile(cfgPath, []byte(configBody), 0o600))

	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), strings.Repeat("very-long-control-root-", 8)))
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), strings.Repeat("very-long-runtime-root-", 8)))

	cc := &CLIContext{
		Logger:       slog.New(slog.DiscardHandler),
		OutputWriter: io.Discard,
		StatusWriter: io.Discard,
		CfgPath:      cfgPath,
	}
	called := false
	cc.syncRunOnceRunner = func(
		context.Context,
		*config.Holder,
		[]*config.ResolvedDrive,
		syncengine.SyncMode,
		syncengine.RunOptions,
		*slog.Logger,
		string,
	) multisync.RunOnceResult {
		called = true
		return multisync.RunOnceResult{}
	}

	err := runSyncCommand(t.Context(), cc, syncCommandOptions{Mode: syncengine.SyncBidirectional})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolve control socket path")
	assert.False(t, called, "one-shot sync owner must stop before engine startup when the socket path is impossible")
}

func TestRunSyncCommand_FailsLoudlyWhenControlSocketPathCannotBeDerivedForWatch(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	syncDir := t.TempDir()
	configBody := fmt.Sprintf(`
["personal:test@example.com"]
sync_dir = %q
`, syncDir)
	require.NoError(t, os.WriteFile(cfgPath, []byte(configBody), 0o600))

	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), strings.Repeat("very-long-control-root-", 8)))
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), strings.Repeat("very-long-runtime-root-", 8)))

	cc := &CLIContext{
		Logger:       slog.New(slog.DiscardHandler),
		OutputWriter: io.Discard,
		StatusWriter: io.Discard,
		CfgPath:      cfgPath,
	}
	called := false
	cc.syncWatchRunner = func(
		context.Context,
		*config.Holder,
		[]string,
		syncengine.SyncMode,
		syncengine.WatchOptions,
		*slog.Logger,
		io.Writer,
		string,
	) error {
		called = true
		return nil
	}

	err := runSyncCommand(t.Context(), cc, syncCommandOptions{
		Mode:  syncengine.SyncBidirectional,
		Watch: true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolve control socket path")
	assert.False(t, called, "watch sync owner must stop before daemon startup when the socket path is impossible")
}

func TestRunSyncCommand_SkipsPausedInvalidDrivesDuringValidation(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	runnableSyncDir := t.TempDir()
	configBody := fmt.Sprintf(`
["personal:runnable@example.com"]
sync_dir = %q

["personal:paused@example.com"]
sync_dir = "./relative-path"
paused = true
`, runnableSyncDir)
	require.NoError(t, os.WriteFile(cfgPath, []byte(configBody), 0o600))

	var status bytes.Buffer
	cc := &CLIContext{
		Logger:       slog.New(slog.DiscardHandler),
		OutputWriter: io.Discard,
		StatusWriter: &status,
		CfgPath:      cfgPath,
	}
	called := false
	cc.syncRunOnceRunner = func(
		_ context.Context,
		_ *config.Holder,
		drives []*config.ResolvedDrive,
		mode syncengine.SyncMode,
		_ syncengine.RunOptions,
		_ *slog.Logger,
		_ string,
	) multisync.RunOnceResult {
		called = true

		results := make([]multisync.DriveStartupResult, 0, len(drives))
		reports := make([]*multisync.DriveReport, 0, len(drives))
		for i := range drives {
			rd := drives[i]
			if rd.Paused {
				results = append(results, multisync.DriveStartupResult{
					SelectionIndex: i,
					CanonicalID:    rd.CanonicalID,
					DisplayName:    rd.DisplayName,
					Status:         multisync.DriveStartupPaused,
				})
				continue
			}

			results = append(results, multisync.DriveStartupResult{
				SelectionIndex: i,
				CanonicalID:    rd.CanonicalID,
				DisplayName:    rd.DisplayName,
				Status:         multisync.DriveStartupRunnable,
			})
			reports = append(reports, &multisync.DriveReport{
				SelectionIndex: i,
				CanonicalID:    rd.CanonicalID,
				DisplayName:    rd.DisplayName,
				Report: &syncengine.Report{
					Mode: mode,
				},
			})
		}

		return multisync.RunOnceResult{
			Startup: multisync.StartupSelectionSummary{Results: results},
			Reports: reports,
		}
	}

	err := runSyncCommand(t.Context(), cc, syncCommandOptions{Mode: syncengine.SyncBidirectional})
	require.NoError(t, err)
	assert.True(t, called)
	assert.Contains(t, status.String(), "Skipped: drive is paused")
}
