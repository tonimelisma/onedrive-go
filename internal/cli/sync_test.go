package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	"github.com/tonimelisma/onedrive-go/testutil"
)

// --- runOnceResultError ---

// Validates: R-6.6.22, R-6.6.28
//
// The drives that could not sync are the one-shot command's error: one
// drive's own error, or a count of the failed drives that keeps the first
// error as its cause.
func TestRunOnceResultError_DriveFailures(t *testing.T) {
	t.Parallel()

	errDelta := fmt.Errorf("delta expired")
	errAuth := fmt.Errorf("auth failed")

	tests := []struct {
		name    string
		reports []*multisync.MountReport
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
			reports: []*multisync.MountReport{
				{Report: &syncengine.Report{Mode: syncengine.SyncBidirectional}},
			},
			wantNil: true,
		},
		{
			name: "one failure",
			reports: []*multisync.MountReport{
				{Err: errDelta},
			},
			wantMsg: "delta expired",
		},
		{
			name: "reset required includes guidance",
			reports: []*multisync.MountReport{
				{
					Identity: testStandaloneMountIdentity(
						driveid.MustCanonicalID("personal:reset@example.com"),
					),
					Err: &syncengine.StateStoreIncompatibleError{
						Reason: syncengine.StateStoreIncompatibleReasonIncompatibleSchema,
					},
				},
			},
			wantMsg: "drive reset-sync-state --drive 'personal:reset@example.com'",
		},
		{
			name: "multi-mount mixed",
			reports: []*multisync.MountReport{
				{Report: &syncengine.Report{}},
				{Err: errDelta},
			},
			wantMsg: "1 of 2 selected drives failed or were skipped (see above)",
		},
		{
			name: "all failures",
			reports: []*multisync.MountReport{
				{Err: errDelta},
				{Err: errAuth},
			},
			wantMsg: "2 of 2 selected drives failed or were skipped (see above)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := multisync.RunOnceResult{Reports: tt.reports}
			for i := range tt.reports {
				tt.reports[i].SelectionIndex = i
				result.Startup.Results = append(result.Startup.Results, multisync.MountStartupResult{
					SelectionIndex: i,
					Status:         multisync.MountStartupRunnable,
				})
			}

			err := runOnceResultError(result, map[string]syncengine.LeftWork{})
			if tt.wantNil {
				assert.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantMsg)
			}
		})
	}
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

func installDryRunSuccessRunner(t *testing.T, cc *CLIContext) *bool {
	t.Helper()

	called := false
	cc.seams.syncRunOnce = func(
		_ context.Context,
		_ *config.Holder,
		drives []*config.ResolvedDrive,
		mode syncengine.SyncMode,
		opts syncengine.RunOptions,
		_ *slog.Logger,
		controlSocketPath string,
	) multisync.RunOnceResult {
		called = true
		require.Len(t, drives, 1)
		assert.Equal(t, syncengine.SyncBidirectional, mode)
		assert.True(t, opts.DryRun)
		assert.NotEmpty(t, controlSocketPath)
		return multisync.RunOnceResult{
			Startup: multisync.StartupSelectionSummary{
				Results: []multisync.MountStartupResult{{
					Identity:    testStandaloneMountIdentity(drives[0].CanonicalID),
					DisplayName: drives[0].DisplayName,
					Status:      multisync.MountStartupRunnable,
				}},
			},
			Reports: []*multisync.MountReport{{
				Identity:    testStandaloneMountIdentity(drives[0].CanonicalID),
				DisplayName: drives[0].DisplayName,
				Report:      &syncengine.Report{Mode: mode, DryRun: true},
			}},
		}
	}

	return &called
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

// Validates: R-2.1.5
func TestDryRunFlagSurfaceOnlySyncCommand(t *testing.T) {
	t.Parallel()

	root := newRootCmdWithWriters(nil, nil)
	var commandsWithDryRun []string
	walkCommandTree(root, func(cmd *cobra.Command) {
		if cmd.LocalFlags().Lookup("dry-run") != nil || cmd.PersistentFlags().Lookup("dry-run") != nil {
			commandsWithDryRun = append(commandsWithDryRun, cmd.CommandPath())
		}
	})

	assert.Equal(t, []string{"onedrive-go sync"}, commandsWithDryRun)
}

func walkCommandTree(cmd *cobra.Command, visit func(*cobra.Command)) {
	if cmd == nil {
		return
	}

	visit(cmd)
	for _, child := range cmd.Commands() {
		walkCommandTree(child, visit)
	}
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
	assert.Contains(t, err.Error(), "all selected mounts are paused")
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
	cc.seams.syncRunOnce = func(
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
				Results: []multisync.MountStartupResult{{
					Identity:    testStandaloneMountIdentity(drives[0].CanonicalID),
					DisplayName: drives[0].DisplayName,
					Status:      multisync.MountStartupRunnable,
				}},
			},
			Reports: []*multisync.MountReport{
				{
					Identity:    testStandaloneMountIdentity(drives[0].CanonicalID),
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

func TestRunSyncCommand_DryRunOpensLogFileAndWarnsOnFailure(t *testing.T) {
	setTestDriveHome(t)

	parentDir := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(parentDir, []byte("x"), 0o600))

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	syncDir := t.TempDir()
	configBody := fmt.Sprintf(`
dry_run = true
log_file = %q

["personal:test@example.com"]
sync_dir = %q
`, filepath.Join(parentDir, "sync.log"), syncDir)
	require.NoError(t, os.WriteFile(cfgPath, []byte(configBody), 0o600))

	var status bytes.Buffer
	cc := &CLIContext{
		Logger:       slog.New(slog.DiscardHandler),
		OutputWriter: io.Discard,
		StatusWriter: &status,
		CfgPath:      cfgPath,
	}
	called := installDryRunSuccessRunner(t, cc)

	err := runSyncCommand(t.Context(), cc, syncCommandOptions{Mode: syncengine.SyncBidirectional})
	require.NoError(t, err)
	assert.True(t, *called)
	assert.Contains(t, status.String(), "cannot open log file")
}

func TestRunSyncCommand_PassesMissingSyncDirToRunOnce(t *testing.T) {
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
	var gotDrives []*config.ResolvedDrive
	cc.seams.syncRunOnce = func(
		_ context.Context,
		_ *config.Holder,
		drives []*config.ResolvedDrive,
		_ syncengine.SyncMode,
		_ syncengine.RunOptions,
		_ *slog.Logger,
		_ string,
	) multisync.RunOnceResult {
		called = true
		gotDrives = drives
		require.Len(t, drives, 1)
		return multisync.RunOnceResult{
			Startup: multisync.StartupSelectionSummary{
				Results: []multisync.MountStartupResult{{
					SelectionIndex: 0,
					Identity:       testStandaloneMountIdentity(drives[0].CanonicalID),
					DisplayName:    drives[0].DisplayName,
					Status:         multisync.MountStartupFatal,
					Err:            fmt.Errorf("%w: %s is missing or is not a folder", syncengine.ErrMountRootUnavailable, drives[0].SyncDir),
				}},
			},
		}
	}

	err := runSyncCommand(t.Context(), cc, syncCommandOptions{Mode: syncengine.SyncBidirectional})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sync folder unavailable")
	assert.True(t, called)
	require.Len(t, gotDrives, 1)
	assert.Equal(t, syncDir, gotDrives[0].SyncDir)

	_, statErr := os.Stat(syncDir)
	assert.True(t, os.IsNotExist(statErr), "sync must not create sync_dir during command startup")
}

func TestRunSyncCommand_DryRunPassesMissingSyncDirWithoutCreatingIt(t *testing.T) {
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
	cc.seams.syncRunOnce = func(
		_ context.Context,
		_ *config.Holder,
		drives []*config.ResolvedDrive,
		_ syncengine.SyncMode,
		opts syncengine.RunOptions,
		_ *slog.Logger,
		_ string,
	) multisync.RunOnceResult {
		called = true
		require.Len(t, drives, 1)
		assert.Equal(t, syncDir, drives[0].SyncDir)
		assert.True(t, opts.DryRun)
		return multisync.RunOnceResult{}
	}

	dryRun := true
	err := runSyncCommand(t.Context(), cc, syncCommandOptions{
		Mode:   syncengine.SyncBidirectional,
		DryRun: &dryRun,
	})
	require.NoError(t, err)
	assert.True(t, called)

	_, statErr := os.Stat(syncDir)
	assert.True(t, os.IsNotExist(statErr), "dry-run must not create sync_dir")
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
	cc.seams.syncRunOnce = func(
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
				Results: []multisync.MountStartupResult{{
					Status: multisync.MountStartupRunnable,
				}},
			},
			Reports: []*multisync.MountReport{{Report: &syncengine.Report{Mode: syncengine.SyncBidirectional}}},
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
		seams: cliSeams{syncWatch: func(
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
		}},
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
	cc.seams.syncRunOnce = func(
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

func TestRunSyncCommand_DryRunFailsWhenControlSocketPathCannotBeDerived(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	syncDir := t.TempDir()
	configBody := fmt.Sprintf(`
dry_run = true

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
	cc.seams.syncRunOnce = func(
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
	assert.False(t, called)
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
	cc.seams.syncWatch = func(
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

func TestRunSyncCommand_PassesPausedInvalidDriveToRunnerAsPaused(t *testing.T) {
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

	var status, output bytes.Buffer
	cc := &CLIContext{
		Logger:       slog.New(slog.DiscardHandler),
		OutputWriter: &output,
		StatusWriter: &status,
		CfgPath:      cfgPath,
	}
	called := false
	cc.seams.syncRunOnce = func(
		_ context.Context,
		_ *config.Holder,
		drives []*config.ResolvedDrive,
		mode syncengine.SyncMode,
		_ syncengine.RunOptions,
		_ *slog.Logger,
		_ string,
	) multisync.RunOnceResult {
		called = true

		results := make([]multisync.MountStartupResult, 0, len(drives))
		reports := make([]*multisync.MountReport, 0, len(drives))
		for i := range drives {
			rd := drives[i]
			if rd.Paused {
				results = append(results, multisync.MountStartupResult{
					SelectionIndex: i,
					Identity:       testStandaloneMountIdentity(rd.CanonicalID),
					DisplayName:    rd.DisplayName,
					Status:         multisync.MountStartupPaused,
				})
				continue
			}

			results = append(results, multisync.MountStartupResult{
				SelectionIndex: i,
				Identity:       testStandaloneMountIdentity(rd.CanonicalID),
				DisplayName:    rd.DisplayName,
				Status:         multisync.MountStartupRunnable,
			})
			reports = append(reports, &multisync.MountReport{
				SelectionIndex: i,
				Identity:       testStandaloneMountIdentity(rd.CanonicalID),
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
	assert.Contains(t, output.String(), "Skipped (paused).")
}

// oneShotDrive is one configured drive of a one-shot sync test and what the
// runner seam hands back for it: a report, a drive error, or a paused drive.
type oneShotDrive struct {
	email  string
	paused bool
	report *syncengine.Report
	err    error
}

// runOneShotWithReports runs `sync` once over the drives through the runner
// seam, which feeds no activity, as the orchestrator's hook would not for a
// drive whose engine reported only counts. It returns stdout and the command
// error.
func runOneShotWithReports(t *testing.T, drives ...oneShotDrive) (string, error) {
	t.Helper()
	setTestDriveHome(t)

	var configBody strings.Builder
	byEmail := make(map[string]oneShotDrive, len(drives))
	for _, drive := range drives {
		fmt.Fprintf(&configBody, "[%q]\nsync_dir = %q\npaused = %t\n\n", "personal:"+drive.email, t.TempDir(), drive.paused)
		byEmail[drive.email] = drive
	}
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(configBody.String()), 0o600))

	var output bytes.Buffer
	cc := &CLIContext{
		Logger:       slog.New(slog.DiscardHandler),
		OutputWriter: &output,
		StatusWriter: io.Discard,
		CfgPath:      cfgPath,
	}
	cc.seams.syncRunOnce = func(
		_ context.Context,
		_ *config.Holder,
		resolved []*config.ResolvedDrive,
		_ syncengine.SyncMode,
		_ syncengine.RunOptions,
		_ *slog.Logger,
		_ string,
	) multisync.RunOnceResult {
		require.Len(t, resolved, len(drives))
		var result multisync.RunOnceResult
		for i, rd := range resolved {
			drive := byEmail[rd.CanonicalID.Email()]
			startup := multisync.MountStartupResult{
				SelectionIndex: i,
				Identity:       testStandaloneMountIdentity(rd.CanonicalID),
				DisplayName:    rd.DisplayName,
				Status:         multisync.MountStartupRunnable,
			}
			if drive.paused {
				startup.Status = multisync.MountStartupPaused
				result.Startup.Results = append(result.Startup.Results, startup)
				continue
			}
			result.Startup.Results = append(result.Startup.Results, startup)
			result.Reports = append(result.Reports, &multisync.MountReport{
				SelectionIndex: i,
				Identity:       startup.Identity,
				DisplayName:    rd.DisplayName,
				Report:         drive.report,
				Err:            drive.err,
			})
		}

		return result
	}

	err := runSyncCommand(t.Context(), cc, syncCommandOptions{Mode: syncengine.SyncBidirectional})

	return output.String(), err
}

// requireSyncLeftWork checks that a one-shot sync failed because it left
// work, with exit status 1 and the message given.
func requireSyncLeftWork(t *testing.T, err error, message string) {
	t.Helper()

	require.ErrorIs(t, err, errSyncLeftWork)
	assert.Equal(t, exitFailure, commandExitCode(err, false))
	assert.Equal(t, message, err.Error())
}

// Validates: R-6.6.28, R-6.6.31
func TestRunSyncCommand_FailedActionsExitNonZero(t *testing.T) {
	out, err := runOneShotWithReports(t, oneShotDrive{email: "test@example.com", report: &syncengine.Report{
		Uploads: 2, Succeeded: 1, Failed: 1, LastPassFailed: 1,
	}})

	requireSyncLeftWork(t, err, "sync left 1 file unsynced; run 'onedrive-go status' for details")
	assert.Contains(t, out, "; 1 failed.")
}

// Validates: R-6.6.28, R-6.6.31
func TestRunSyncCommand_WorkLeftForNextSyncExitsNonZero(t *testing.T) {
	out, err := runOneShotWithReports(t, oneShotDrive{email: "test@example.com", report: &syncengine.Report{
		Uploads: 3, Succeeded: 1, LeftForNextSync: 2,
	}})

	requireSyncLeftWork(t, err, "sync left 2 files unsynced; run 'onedrive-go status' for details")
	assert.Contains(t, out, "; 2 will be retried on the next sync.")
}

// Validates: R-6.6.28, R-6.6.31
//
// A run that itself did nothing still left work when the store holds issues
// that keep a file unsynced (names OneDrive refuses): no plan acts on such a
// path, and `status` lists it, so the exit status says so on every run.
func TestRunSyncCommand_StillOpenConditionsExitNonZero(t *testing.T) {
	out, err := runOneShotWithReports(t, oneShotDrive{email: "test@example.com", report: &syncengine.Report{
		UnsyncedIssues: 3,
	}})

	requireSyncLeftWork(t, err, "sync left 3 files unsynced; run 'onedrive-go status' for details")
	assert.Contains(t, out, "; 3 still not synced (see 'onedrive-go status').")
	assert.NotContains(t, out, "Up to date")
}

// Validates: R-6.6.28
//
// A plan whose actions and dependencies disagree runs none of it and
// reports every action failed with no activity; the run exits 1.
func TestRunSyncCommand_PlanInvariantViolationExitsNonZero(t *testing.T) {
	_, err := runOneShotWithReports(t, oneShotDrive{email: "test@example.com", report: &syncengine.Report{
		Uploads:        2,
		Failed:         2,
		LastPassFailed: 2,
		Errors:         []error{errors.New("plan invariant violation: 2 actions but 1 deps")},
	}})

	requireSyncLeftWork(t, err, "sync left 2 files unsynced; run 'onedrive-go status' for details")
}

// Validates: R-6.6.28
func TestRunSyncCommand_DeferredByModeExitsZero(t *testing.T) {
	out, err := runOneShotWithReports(t, oneShotDrive{email: "test@example.com", report: &syncengine.Report{
		Mode:           syncengine.SyncDownloadOnly,
		DeferredByMode: syncengine.DeferredCounts{Uploads: 3},
	}})

	require.NoError(t, err)
	assert.Contains(t, out, "Held back by --download-only: 3 uploads.")
}

// Validates: R-6.6.28
func TestRunSyncCommand_HeldDeletesExitZero(t *testing.T) {
	_, err := runOneShotWithReports(t, oneShotDrive{email: "test@example.com", report: &syncengine.Report{
		HeldRemoteDeletes: 60,
	}})

	require.NoError(t, err, "deletes held for --confirm-deletes are a deliberate hold")
}

// Validates: R-6.6.28
func TestRunSyncCommand_ConflictCopiesExitZero(t *testing.T) {
	_, err := runOneShotWithReports(t, oneShotDrive{email: "test@example.com", report: &syncengine.Report{
		ConflictCopies: 1, Succeeded: 1,
	}})

	require.NoError(t, err, "both versions are kept and synced")
}

// Validates: R-6.6.28
func TestRunSyncCommand_DryRunExitsZero(t *testing.T) {
	out, err := runOneShotWithReports(t, oneShotDrive{email: "test@example.com", report: &syncengine.Report{
		DryRun:         true,
		Uploads:        2,
		UnsyncedIssues: 1,
	}})

	require.NoError(t, err, "a dry run attempted nothing")
	assert.Contains(t, out, "Dry run, nothing was changed. Would upload 2 files.")
}

// Validates: R-6.6.28
func TestRunSyncCommand_PausedDriveBesideASyncedDriveExitsZero(t *testing.T) {
	out, err := runOneShotWithReports(t,
		oneShotDrive{email: "paused@example.com", paused: true},
		oneShotDrive{email: "synced@example.com", report: &syncengine.Report{}},
	)

	require.NoError(t, err)
	assert.Contains(t, out, "Skipped (paused).")
}

// Validates: R-6.6.28, R-6.6.22
func TestRunSyncCommand_MultiDriveCountsTheDrivesThatLeftWork(t *testing.T) {
	out, err := runOneShotWithReports(t,
		oneShotDrive{email: "clean@example.com", report: &syncengine.Report{}},
		oneShotDrive{email: "left@example.com", report: &syncengine.Report{Uploads: 1, Failed: 1, LastPassFailed: 1}},
	)

	requireSyncLeftWork(t, err, "1 of 2 selected drives left files unsynced (see above)")
	assert.Contains(t, out, "Up to date")
	assert.Contains(t, out, "; 1 failed.")
}

// Validates: R-6.6.28
//
// The live e2e suite tells a pass that left only what its test account held
// before the suite started from a pass that left work of its own, and it does
// so by reading this command's output (testutil.StandingSyncConditions). This
// pins the wording it reads: for a run over several drives of which one left
// only items still not synced, settling the output gives exactly what the
// command prints for the same run without them.
func TestRunSyncCommand_StandingConditionOutputSettlesToTheRunWithoutIt(t *testing.T) {
	cases := []struct {
		name string
		work syncengine.Report
	}{
		{name: "nothing to do", work: syncengine.Report{}},
		{name: "work done", work: syncengine.Report{Downloads: 2, Succeeded: 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			without := tc.work
			wantOut, err := runOneShotWithReports(t,
				oneShotDrive{email: "clean@example.com", report: &syncengine.Report{}},
				oneShotDrive{email: "standing@example.com", report: &without},
			)
			require.NoError(t, err)

			with := tc.work
			with.UnsyncedIssues = 1
			out, err := runOneShotWithReports(t,
				oneShotDrive{email: "clean@example.com", report: &syncengine.Report{}},
				oneShotDrive{email: "standing@example.com", report: &with},
			)
			requireSyncLeftWork(t, err, "1 of 2 selected drives left files unsynced (see above)")

			var standing testutil.StandingSyncConditions
			standing.RecordBaseline(out)
			require.Equal(t, 1, standing.Mounts())
			require.Equal(t, 1, standing.Items())
			require.Zero(t, standing.Unattributed())

			settledOut, settledErr, settled := standing.Settle(out, commandFailureText(err, false)+"\n")
			require.True(t, settled)
			assert.Equal(t, withoutSyncingLines(wantOut), withoutSyncingLines(settledOut))
			assert.Contains(t, settledOut, "Syncing ", "the lines that are not summaries stay")
			assert.Empty(t, settledErr)
		})
	}
}

// withoutSyncingLines drops the "Syncing <dir> with <drive>..." lines, which
// name the temporary sync directory of each run.
func withoutSyncingLines(out string) string {
	var kept strings.Builder
	for line := range strings.SplitAfterSeq(out, "\n") {
		if !strings.HasPrefix(line, "Syncing ") {
			kept.WriteString(line)
		}
	}

	return kept.String()
}

// Validates: R-6.6.28, R-6.6.31
//
// A drive that could not sync because sign-in is required keeps exit 77 even
// when another drive left work: running again unchanged cannot fix it.
func TestRunSyncCommand_ADriveErrorWinsOverLeftWork(t *testing.T) {
	_, err := runOneShotWithReports(t,
		oneShotDrive{email: "left@example.com", report: &syncengine.Report{Uploads: 1, Failed: 1, LastPassFailed: 1}},
		oneShotDrive{email: "signin@example.com", err: fmt.Errorf("session: %w", graph.ErrNotLoggedIn)},
	)

	require.Error(t, err)
	require.NotErrorIs(t, err, errSyncLeftWork)
	assert.Equal(t, exitSignInRequired, commandExitCode(err, false))
}
