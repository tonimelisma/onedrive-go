package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

// Global flag reset pattern: newRootCmd() binds flags via StringVar/BoolVar,
// which reset the global flag variables to their zero values. Tests must either:
//   - Set globals AFTER newRootCmd() returns (direct function tests), or
//   - Use cmd.SetArgs() + cmd.Execute() to let Cobra parse flags (integration tests).
//
// Setting a global before newRootCmd() and expecting it to survive is a bug.

// --- buildLogger tests ---

func TestBuildLogger_Default(t *testing.T) {
	oldVerbose := flagVerbose
	oldDebug := flagDebug
	oldQuiet := flagQuiet

	t.Cleanup(func() {
		flagVerbose = oldVerbose
		flagDebug = oldDebug
		flagQuiet = oldQuiet
	})

	flagVerbose = false
	flagDebug = false
	flagQuiet = false

	// nil config = bootstrap mode (pre-config).
	logger := buildLogger(nil)

	// Default level is Warn.
	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelWarn))
	assert.False(t, logger.Handler().Enabled(context.Background(), slog.LevelInfo))
}

func TestBuildLogger_Verbose(t *testing.T) {
	oldVerbose := flagVerbose
	oldDebug := flagDebug
	oldQuiet := flagQuiet

	t.Cleanup(func() {
		flagVerbose = oldVerbose
		flagDebug = oldDebug
		flagQuiet = oldQuiet
	})

	flagVerbose = true
	flagDebug = false
	flagQuiet = false

	logger := buildLogger(nil)

	// --verbose sets Info.
	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelInfo))
	assert.False(t, logger.Handler().Enabled(context.Background(), slog.LevelDebug))
}

func TestBuildLogger_Debug(t *testing.T) {
	oldVerbose := flagVerbose
	oldDebug := flagDebug
	oldQuiet := flagQuiet

	t.Cleanup(func() {
		flagVerbose = oldVerbose
		flagDebug = oldDebug
		flagQuiet = oldQuiet
	})

	flagVerbose = false
	flagDebug = true
	flagQuiet = false

	logger := buildLogger(nil)

	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelDebug))
}

func TestBuildLogger_ConfigDebug(t *testing.T) {
	oldVerbose := flagVerbose
	oldDebug := flagDebug
	oldQuiet := flagQuiet

	t.Cleanup(func() {
		flagVerbose = oldVerbose
		flagDebug = oldDebug
		flagQuiet = oldQuiet
	})

	cfg := &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogLevel: "debug"},
	}
	flagVerbose = false
	flagDebug = false
	flagQuiet = false

	logger := buildLogger(cfg)

	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelDebug))
}

func TestBuildLogger_VerboseOverrides(t *testing.T) {
	oldVerbose := flagVerbose
	oldDebug := flagDebug
	oldQuiet := flagQuiet

	t.Cleanup(func() {
		flagVerbose = oldVerbose
		flagDebug = oldDebug
		flagQuiet = oldQuiet
	})

	// Config says error, but --verbose should override to Info.
	cfg := &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogLevel: "error"},
	}
	flagVerbose = true
	flagDebug = false
	flagQuiet = false

	logger := buildLogger(cfg)

	// --verbose enables Info level, but not Debug.
	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelInfo))
	assert.False(t, logger.Handler().Enabled(context.Background(), slog.LevelDebug))
}

func TestBuildLogger_QuietOverrides(t *testing.T) {
	oldVerbose := flagVerbose
	oldDebug := flagDebug
	oldQuiet := flagQuiet

	t.Cleanup(func() {
		flagVerbose = oldVerbose
		flagDebug = oldDebug
		flagQuiet = oldQuiet
	})

	// --quiet sets Error level.
	flagVerbose = false
	flagDebug = false
	flagQuiet = true

	logger := buildLogger(nil)

	// Error is enabled, but warn should not be.
	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelError))
	assert.False(t, logger.Handler().Enabled(context.Background(), slog.LevelWarn))
}

func TestBuildLogger_DebugOverrides(t *testing.T) {
	oldVerbose := flagVerbose
	oldDebug := flagDebug
	oldQuiet := flagQuiet

	t.Cleanup(func() {
		flagVerbose = oldVerbose
		flagDebug = oldDebug
		flagQuiet = oldQuiet
	})

	// Config says error, but --debug should override to Debug.
	cfg := &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogLevel: "error"},
	}
	flagVerbose = false
	flagDebug = true
	flagQuiet = false

	logger := buildLogger(cfg)

	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelDebug))
}

func TestBuildLogger_ConfigInfo(t *testing.T) {
	oldVerbose := flagVerbose
	oldDebug := flagDebug
	oldQuiet := flagQuiet

	t.Cleanup(func() {
		flagVerbose = oldVerbose
		flagDebug = oldDebug
		flagQuiet = oldQuiet
	})

	// Config log_level = "info" should set Info level.
	cfg := &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogLevel: "info"},
	}
	flagVerbose = false
	flagDebug = false
	flagQuiet = false

	logger := buildLogger(cfg)

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
	// Uses "status" because it has skipConfigAnnotation, so PersistentPreRunE
	// is a no-op. This avoids loadConfig failures on CI (no config file)
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
	// without error, because they have the skipConfigAnnotation.
	skipCmds := []string{"login", "logout", "whoami", "status"}
	for _, name := range skipCmds {
		t.Run(name, func(t *testing.T) {
			sub, _, err := cmd.Find([]string{name})
			require.NoError(t, err)

			err = cmd.PersistentPreRunE(sub, nil)
			assert.NoError(t, err, "%s should skip config loading", name)
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
		{"sync"},
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

// --- loadConfig tests ---

func TestLoadConfig_ValidTOML(t *testing.T) {
	oldConfigPath := flagConfigPath

	t.Cleanup(func() {
		flagConfigPath = oldConfigPath
	})

	tmpDir := t.TempDir()
	cfgFile := filepath.Join(tmpDir, "config.toml")

	// Drive sections use top-level TOML keys with canonical IDs (containing ":").
	tomlContent := `["personal:test@example.com"]
alias = "test"
sync_dir = "` + tmpDir + `/OneDrive"
`
	err := os.WriteFile(cfgFile, []byte(tomlContent), 0o600)
	require.NoError(t, err)

	cmd := newRootCmd()
	cmd.SetContext(context.Background())
	flagConfigPath = cfgFile

	err = loadConfig(cmd)
	require.NoError(t, err)

	cc := cliContextFrom(cmd.Context())
	require.NotNil(t, cc)
	assert.NotNil(t, cc.Logger)

	assert.Equal(t, "personal:test@example.com", cc.Cfg.CanonicalID.String())
}

func TestLoadConfig_MissingFile_ZeroConfig(t *testing.T) {
	oldConfigPath := flagConfigPath

	t.Cleanup(func() {
		flagConfigPath = oldConfigPath
	})

	tmpDir := t.TempDir()
	// Save config path before newRootCmd(), which resets flagConfigPath to "".
	cfgPath := filepath.Join(tmpDir, "nonexistent.toml")

	// Zero-config mode: no config file, but --drive is set with a canonical ID.
	// The resolver allows this because the selector contains ":" (canonical format),
	// even when no drives are configured in the file.
	// We use Execute with the ls subcommand so Cobra properly merges persistent
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
	assert.Equal(t, "personal:zeroconfig@example.com", cc.Cfg.CanonicalID.String())
}
