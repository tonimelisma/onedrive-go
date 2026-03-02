package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

// --- buildLogger tests ---

func TestBuildLogger_Default(t *testing.T) {
	flags := CLIFlags{}

	// nil config = bootstrap mode (pre-config).
	logger := buildLogger(nil, flags)

	// Default level is Warn.
	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelWarn))
	assert.False(t, logger.Handler().Enabled(context.Background(), slog.LevelInfo))
}

func TestBuildLogger_Verbose(t *testing.T) {
	flags := CLIFlags{Verbose: true}

	logger := buildLogger(nil, flags)

	// --verbose sets Info.
	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelInfo))
	assert.False(t, logger.Handler().Enabled(context.Background(), slog.LevelDebug))
}

func TestBuildLogger_Debug(t *testing.T) {
	flags := CLIFlags{Debug: true}

	logger := buildLogger(nil, flags)

	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelDebug))
}

func TestBuildLogger_ConfigDebug(t *testing.T) {
	cfg := &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogLevel: "debug"},
	}
	flags := CLIFlags{}

	logger := buildLogger(cfg, flags)

	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelDebug))
}

func TestBuildLogger_VerboseOverrides(t *testing.T) {
	// Config says error, but --verbose should override to Info.
	cfg := &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogLevel: "error"},
	}
	flags := CLIFlags{Verbose: true}

	logger := buildLogger(cfg, flags)

	// --verbose enables Info level, but not Debug.
	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelInfo))
	assert.False(t, logger.Handler().Enabled(context.Background(), slog.LevelDebug))
}

func TestBuildLogger_QuietOverrides(t *testing.T) {
	// --quiet sets Error level.
	flags := CLIFlags{Quiet: true}

	logger := buildLogger(nil, flags)

	// Error is enabled, but warn should not be.
	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelError))
	assert.False(t, logger.Handler().Enabled(context.Background(), slog.LevelWarn))
}

func TestBuildLogger_DebugOverrides(t *testing.T) {
	// Config says error, but --debug should override to Debug.
	cfg := &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogLevel: "error"},
	}
	flags := CLIFlags{Debug: true}

	logger := buildLogger(cfg, flags)

	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelDebug))
}

func TestBuildLogger_ConfigInfo(t *testing.T) {
	// Config log_level = "info" should set Info level.
	cfg := &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogLevel: "info"},
	}
	flags := CLIFlags{}

	logger := buildLogger(cfg, flags)

	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelInfo))
	assert.False(t, logger.Handler().Enabled(context.Background(), slog.LevelDebug))
}

// --- cliContextFrom tests ---

func TestCliContextFrom_NilContext(t *testing.T) {
	ctx := context.Background()
	cc := cliContextFrom(ctx)
	assert.Nil(t, cc)
}

func TestCliContextFrom_WithCLIContext(t *testing.T) {
	expected := &CLIContext{
		Cfg:    &config.ResolvedDrive{SyncDir: "/test"},
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	ctx := context.WithValue(context.Background(), cliContextKey{}, expected)
	cc := cliContextFrom(ctx)
	assert.Equal(t, expected, cc)
	assert.Equal(t, "/test", cc.Cfg.SyncDir)
	assert.NotNil(t, cc.Logger)
}

// --- Cobra structure tests ---

func TestNewRootCmd_Subcommands(t *testing.T) {
	cmd := newRootCmd()

	expected := []string{"login", "logout", "whoami", "status", "drive", "ls", "get", "put", "rm", "mkdir", "stat"}
	for _, name := range expected {
		found := false

		for _, sub := range cmd.Commands() {
			if sub.Name() == name {
				found = true

				break
			}
		}

		assert.True(t, found, "expected subcommand %q not found", name)
	}
}

func TestNewRootCmd_PersistentFlags(t *testing.T) {
	cmd := newRootCmd()

	expectedFlags := []string{"config", "account", "drive", "json", "verbose", "debug", "quiet"}
	for _, name := range expectedFlags {
		flag := cmd.PersistentFlags().Lookup(name)
		assert.NotNil(t, flag, "expected persistent flag %q not found", name)
	}
}

func TestNewRootCmd_MutualExclusivity(t *testing.T) {
	// Cobra enforces mutual exclusivity during Execute(). Verify that
	// combining --verbose/--debug/--quiet produces an error.
	// Uses "status" because it has skipConfigAnnotation, so Phase 2
	// is skipped. This avoids loadAndResolve failures on CI (no config file)
	// masking the mutual exclusivity error.
	pairs := [][]string{
		{"--verbose", "--debug"},
		{"--verbose", "--quiet"},
		{"--debug", "--quiet"},
	}

	for _, flags := range pairs {
		t.Run(flags[0]+"_"+flags[1], func(t *testing.T) {
			cmd := newRootCmd()
			cmd.SetArgs(append(flags, "status"))

			err := cmd.Execute()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "none of the others can be")
		})
	}
}

func TestNewRootCmd_AuthSkipsConfig(t *testing.T) {
	cmd := newRootCmd()

	// Auth and account management commands should pass through PersistentPreRunE
	// without error, because they have the skipConfigAnnotation (Phase 2 skipped).
	skipCmds := []string{"login", "logout", "whoami", "status"}
	for _, name := range skipCmds {
		t.Run(name, func(t *testing.T) {
			sub, _, err := cmd.Find([]string{name})
			require.NoError(t, err)

			sub.SetContext(context.Background())

			err = cmd.PersistentPreRunE(sub, nil)
			assert.NoError(t, err, "%s should skip config loading", name)

			// Verify CLIContext is populated (Phase 1 runs for all commands).
			cc := cliContextFrom(sub.Context())
			assert.NotNil(t, cc, "CLIContext should be populated for %s", name)
			assert.NotNil(t, cc.Logger, "Logger should be populated for %s", name)
			assert.NotEmpty(t, cc.CfgPath, "CfgPath should be populated for %s", name)
			// Env is always populated in Phase 1 (even if both fields are empty).
			assert.IsType(t, config.EnvOverrides{}, cc.Env, "Env should be populated for %s", name)
			assert.Nil(t, cc.Cfg, "Cfg should be nil for auth command %s", name)
		})
	}
}

func TestNewRootCmd_DriveSubcommands(t *testing.T) {
	cmd := newRootCmd()

	// Find the "drive" command and verify its subcommands.
	driveSub, _, err := cmd.Find([]string{"drive"})
	require.NoError(t, err)
	require.Equal(t, "drive", driveSub.Name())

	expectedSubs := []string{"add", "remove"}
	for _, name := range expectedSubs {
		found := false

		for _, sub := range driveSub.Commands() {
			if sub.Name() == name {
				found = true

				break
			}
		}

		assert.True(t, found, "expected drive subcommand %q not found", name)
	}
}

func TestNewRootCmd_DriveSubcommandsSkipConfig(t *testing.T) {
	cmd := newRootCmd()

	// drive, drive add, and drive remove should skip config loading via annotation.
	driveSubCmds := []struct {
		args []string
		name string
	}{
		{[]string{"drive"}, "drive"},
		{[]string{"drive", "add"}, "drive add"},
		{[]string{"drive", "remove"}, "drive remove"},
	}

	for _, tc := range driveSubCmds {
		t.Run(tc.name, func(t *testing.T) {
			sub, _, err := cmd.Find(tc.args)
			require.NoError(t, err)

			sub.SetContext(context.Background())

			err = cmd.PersistentPreRunE(sub, nil)
			assert.NoError(t, err, "%s should skip config loading", tc.name)
		})
	}
}

// --- annotation-based skip config ---

func TestAnnotationBasedSkipConfig(t *testing.T) {
	cmd := newRootCmd()

	// All commands that should skip config must have the annotation.
	skipPaths := [][]string{
		{"login"},
		{"logout"},
		{"whoami"},
		{"status"},
		{"drive"},
		{"drive", "add"},
		{"drive", "remove"},
		{"sync"},
	}

	for _, args := range skipPaths {
		sub, _, err := cmd.Find(args)
		require.NoError(t, err)

		assert.Equal(t, "true", sub.Annotations[skipConfigAnnotation],
			"command %q should have skipConfig annotation", sub.CommandPath())
	}

	// Commands that require config should NOT have the annotation.
	configPaths := [][]string{
		{"ls"},
		{"get"},
		{"put"},
		{"rm"},
		{"mkdir"},
		{"stat"},
		{"conflicts"},
		{"verify"},
		{"resolve"},
	}

	for _, args := range configPaths {
		sub, _, err := cmd.Find(args)
		require.NoError(t, err)

		assert.Empty(t, sub.Annotations[skipConfigAnnotation],
			"command %q should NOT have skipConfig annotation", sub.CommandPath())
	}
}

// --- defaultHTTPClient tests ---

func TestDefaultHTTPClient_HasTimeout(t *testing.T) {
	client := defaultHTTPClient()
	assert.Equal(t, httpClientTimeout, client.Timeout)
}

func TestTransferHTTPClient_NoTimeout(t *testing.T) {
	client := transferHTTPClient()
	assert.Zero(t, client.Timeout)
}

// --- loadAndResolve tests ---

func TestLoadAndResolve_ValidTOML(t *testing.T) {
	tmpDir := t.TempDir()
	cfgFile := filepath.Join(tmpDir, "config.toml")

	// Drive sections use top-level TOML keys with canonical IDs (containing ":").
	tomlContent := `["personal:test@example.com"]
display_name = "test"
sync_dir = "` + tmpDir + `/OneDrive"
`
	err := os.WriteFile(cfgFile, []byte(tomlContent), 0o600)
	require.NoError(t, err)

	flags := CLIFlags{ConfigPath: cfgFile}
	logger := buildLogger(nil, flags)

	cmd := newRootCmd()
	cmd.SetContext(context.Background())

	resolved, rawCfg, err := loadAndResolve(cmd, flags, config.EnvOverrides{}, logger)
	require.NoError(t, err)
	assert.NotNil(t, resolved)
	assert.NotNil(t, rawCfg)
	assert.Equal(t, "personal:test@example.com", resolved.CanonicalID.String())
}

func TestLoadAndResolve_MissingFile_ZeroConfig(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "nonexistent.toml")

	// Zero-config mode: no config file, but --drive is set with a canonical ID.
	// Uses Execute with the ls subcommand so Cobra properly merges persistent
	// flags and marks --drive as changed -- matching real CLI invocation.
	cmd := newRootCmd()
	cmd.SetArgs([]string{"--drive", "personal:zeroconfig@example.com", "--config", cfgPath, "ls"})

	// ls will fail (no token), but PersistentPreRunE should succeed and populate config.
	_ = cmd.Execute()

	// After Execute, find the ls subcommand to get its context.
	lsSub, _, err := cmd.Find([]string{"ls"})
	require.NoError(t, err)

	cc := cliContextFrom(lsSub.Context())
	require.NotNil(t, cc)
	assert.NotNil(t, cc.Logger)
	assert.NotNil(t, cc.Cfg)
	assert.Equal(t, "personal:zeroconfig@example.com", cc.Cfg.CanonicalID.String())
}

// --- mustCLIContext tests ---

func TestMustCLIContext_Panics(t *testing.T) {
	assert.PanicsWithValue(t,
		"BUG: CLIContext not found in context — ensure the command "+
			"does not skip config loading (no skipConfigAnnotation) or "+
			"explicitly loads config in its RunE",
		func() { mustCLIContext(context.Background()) },
	)
}

func TestMustCLIContext_Returns(t *testing.T) {
	expected := &CLIContext{
		Cfg:    &config.ResolvedDrive{SyncDir: "/must-test"},
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	ctx := context.WithValue(context.Background(), cliContextKey{}, expected)
	cc := mustCLIContext(ctx)
	assert.Equal(t, expected, cc)
	assert.Equal(t, "/must-test", cc.Cfg.SyncDir)
}

// --- CLIFlags tests ---

func TestCLIFlags_PopulatedByPersistentPreRunE(t *testing.T) {
	// Verify that PersistentPreRunE populates CLIFlags for auth commands.
	cmd := newRootCmd()
	cmd.SetArgs([]string{"--verbose", "status"})

	_ = cmd.Execute()

	statusSub, _, err := cmd.Find([]string{"status"})
	require.NoError(t, err)

	cc := cliContextFrom(statusSub.Context())
	require.NotNil(t, cc)
	assert.True(t, cc.Flags.Verbose)
}

// --- loadAndResolve error path tests (B-232) ---

func TestLoadAndResolve_InvalidTOML(t *testing.T) {
	tmpDir := t.TempDir()
	cfgFile := filepath.Join(tmpDir, "config.toml")

	err := os.WriteFile(cfgFile, []byte("{{invalid toml"), 0o600)
	require.NoError(t, err)

	flags := CLIFlags{ConfigPath: cfgFile}
	logger := buildLogger(nil, flags)

	cmd := newRootCmd()
	cmd.SetContext(context.Background())

	_, _, err = loadAndResolve(cmd, flags, config.EnvOverrides{}, logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "loading config")
}

func TestLoadAndResolve_AmbiguousDrive(t *testing.T) {
	tmpDir := t.TempDir()
	cfgFile := filepath.Join(tmpDir, "config.toml")

	// Two drives — no --drive flag → ambiguous.
	tomlContent := `["personal:alice@example.com"]
sync_dir = "` + tmpDir + `/one"

["personal:bob@example.com"]
sync_dir = "` + tmpDir + `/two"
`
	err := os.WriteFile(cfgFile, []byte(tomlContent), 0o600)
	require.NoError(t, err)

	flags := CLIFlags{ConfigPath: cfgFile}
	logger := buildLogger(nil, flags)

	cmd := newRootCmd()
	cmd.SetContext(context.Background())

	_, _, err = loadAndResolve(cmd, flags, config.EnvOverrides{}, logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "loading config")
}

func TestBuildLogger_UnknownLogLevel(t *testing.T) {
	cfg := &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogLevel: "bogus"},
	}
	flags := CLIFlags{}

	// Captures stderr warning for unknown log level.
	logger := buildLogger(cfg, flags)

	// Should fall back to warn level.
	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelWarn))
	assert.False(t, logger.Handler().Enabled(context.Background(), slog.LevelInfo))
}

// --- CLIContext.Statusf tests (B-288) ---

func TestCLIContext_Statusf_Quiet(t *testing.T) {
	t.Parallel()

	cc := &CLIContext{Flags: CLIFlags{Quiet: true}}
	// Should not panic. Output suppressed.
	cc.Statusf("should not appear: %d\n", 42)
}

func TestCLIContext_Statusf_Normal(t *testing.T) {
	t.Parallel()

	cc := &CLIContext{Flags: CLIFlags{Quiet: false}}
	// Should not panic. Output goes to stderr.
	cc.Statusf("status message: %s\n", "ok")
}

// --- B-296: sync command log_level fix ---

// TestBuildLogger_FromRawConfigLogLevel verifies the pattern used to apply
// config-file log_level in the sync command. sync uses skipConfigAnnotation
// for multi-drive resolution, so Phase 2 logger construction is skipped.
// runSync rebuilds the logger from rawCfg.LoggingConfig after loading config.
// This test ensures that a ResolvedDrive with only LoggingConfig populated
// (and all other fields at zero value) correctly applies the log level.
func TestBuildLogger_FromRawConfigLogLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		logLevel  string
		wantInfo  bool
		wantDebug bool
	}{
		{
			name:      "info level",
			logLevel:  "info",
			wantInfo:  true,
			wantDebug: false,
		},
		{
			name:      "debug level",
			logLevel:  "debug",
			wantInfo:  true,
			wantDebug: true,
		},
		{
			name:      "warn level",
			logLevel:  "warn",
			wantInfo:  false,
			wantDebug: false,
		},
		{
			name:      "empty level falls back to warn",
			logLevel:  "",
			wantInfo:  false,
			wantDebug: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Mimic the sync command pattern: only LoggingConfig is populated,
			// all other ResolvedDrive fields are zero values.
			rd := &config.ResolvedDrive{
				LoggingConfig: config.LoggingConfig{LogLevel: tc.logLevel},
			}
			logger := buildLogger(rd, CLIFlags{})

			assert.Equal(t, tc.wantInfo,
				logger.Handler().Enabled(context.Background(), slog.LevelInfo),
				"Info enabled mismatch for log_level=%q", tc.logLevel)
			assert.Equal(t, tc.wantDebug,
				logger.Handler().Enabled(context.Background(), slog.LevelDebug),
				"Debug enabled mismatch for log_level=%q", tc.logLevel)
		})
	}
}

// --- errVerifyMismatch tests ---

func TestErrVerifyMismatch_IsSentinel(t *testing.T) {
	// Verify the sentinel error is usable with errors.Is.
	wrapped := fmt.Errorf("wrapped: %w", errVerifyMismatch)
	assert.ErrorIs(t, wrapped, errVerifyMismatch)
}
