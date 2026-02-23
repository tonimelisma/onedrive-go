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

// --- bootstrapLogger tests ---

func TestBootstrapLogger_Default(t *testing.T) {
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

	logger := bootstrapLogger()

	// Default level is Warn.
	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelWarn))
	assert.False(t, logger.Handler().Enabled(context.Background(), slog.LevelInfo))
}

func TestBootstrapLogger_Verbose(t *testing.T) {
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

	logger := bootstrapLogger()

	// --verbose sets Info.
	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelInfo))
	assert.False(t, logger.Handler().Enabled(context.Background(), slog.LevelDebug))
}

func TestBootstrapLogger_Debug(t *testing.T) {
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

	logger := bootstrapLogger()

	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelDebug))
}

// --- buildLogger tests ---

func TestBuildLogger_Default(t *testing.T) {
	oldCfg := resolvedCfg
	oldVerbose := flagVerbose
	oldDebug := flagDebug
	oldQuiet := flagQuiet

	t.Cleanup(func() {
		resolvedCfg = oldCfg
		flagVerbose = oldVerbose
		flagDebug = oldDebug
		flagQuiet = oldQuiet
	})

	resolvedCfg = nil
	flagVerbose = false
	flagDebug = false
	flagQuiet = false

	logger := buildLogger()

	// Default level is Warn: Warn enabled, Info not.
	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelWarn))
	assert.False(t, logger.Handler().Enabled(context.Background(), slog.LevelInfo))
}

func TestBuildLogger_ConfigDebug(t *testing.T) {
	oldCfg := resolvedCfg
	oldVerbose := flagVerbose
	oldDebug := flagDebug
	oldQuiet := flagQuiet

	t.Cleanup(func() {
		resolvedCfg = oldCfg
		flagVerbose = oldVerbose
		flagDebug = oldDebug
		flagQuiet = oldQuiet
	})

	resolvedCfg = &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogLevel: "debug"},
	}
	flagVerbose = false
	flagDebug = false
	flagQuiet = false

	logger := buildLogger()

	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelDebug))
}

func TestBuildLogger_VerboseOverrides(t *testing.T) {
	oldCfg := resolvedCfg
	oldVerbose := flagVerbose
	oldDebug := flagDebug
	oldQuiet := flagQuiet

	t.Cleanup(func() {
		resolvedCfg = oldCfg
		flagVerbose = oldVerbose
		flagDebug = oldDebug
		flagQuiet = oldQuiet
	})

	// Config says error, but --verbose should override to Info.
	resolvedCfg = &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogLevel: "error"},
	}
	flagVerbose = true
	flagDebug = false
	flagQuiet = false

	logger := buildLogger()

	// --verbose enables Info level, but not Debug.
	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelInfo))
	assert.False(t, logger.Handler().Enabled(context.Background(), slog.LevelDebug))
}

func TestBuildLogger_QuietOverrides(t *testing.T) {
	oldCfg := resolvedCfg
	oldVerbose := flagVerbose
	oldDebug := flagDebug
	oldQuiet := flagQuiet

	t.Cleanup(func() {
		resolvedCfg = oldCfg
		flagVerbose = oldVerbose
		flagDebug = oldDebug
		flagQuiet = oldQuiet
	})

	// --quiet sets Error level.
	resolvedCfg = nil
	flagVerbose = false
	flagDebug = false
	flagQuiet = true

	logger := buildLogger()

	// Error is enabled, but warn should not be.
	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelError))
	assert.False(t, logger.Handler().Enabled(context.Background(), slog.LevelWarn))
}

func TestBuildLogger_DebugOverrides(t *testing.T) {
	oldCfg := resolvedCfg
	oldVerbose := flagVerbose
	oldDebug := flagDebug
	oldQuiet := flagQuiet

	t.Cleanup(func() {
		resolvedCfg = oldCfg
		flagVerbose = oldVerbose
		flagDebug = oldDebug
		flagQuiet = oldQuiet
	})

	// Config says error, but --debug should override to Debug.
	resolvedCfg = &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogLevel: "error"},
	}
	flagVerbose = false
	flagDebug = true
	flagQuiet = false

	logger := buildLogger()

	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelDebug))
}

func TestBuildLogger_ConfigInfo(t *testing.T) {
	oldCfg := resolvedCfg
	oldVerbose := flagVerbose
	oldDebug := flagDebug
	oldQuiet := flagQuiet

	t.Cleanup(func() {
		resolvedCfg = oldCfg
		flagVerbose = oldVerbose
		flagDebug = oldDebug
		flagQuiet = oldQuiet
	})

	// Config log_level = "info" should set Info level.
	resolvedCfg = &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogLevel: "info"},
	}
	flagVerbose = false
	flagDebug = false
	flagQuiet = false

	logger := buildLogger()

	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelInfo))
	assert.False(t, logger.Handler().Enabled(context.Background(), slog.LevelDebug))
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
	// Uses "status" because it's in skipConfigCommands, so PersistentPreRunE
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
	// without error, because they skip the four-layer config resolution.
	// Uses CommandPath() for matching, so we test with the full command paths.
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

	// drive, drive add, and drive remove should skip config loading via PersistentPreRunE.
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

// --- defaultHTTPClient tests ---

func TestDefaultHTTPClient_HasTimeout(t *testing.T) {
	client := defaultHTTPClient()
	assert.Equal(t, httpClientTimeout, client.Timeout)
}

// --- skipConfigCommands uses CommandPath ---

func TestSkipConfigCommands_UsesCommandPath(t *testing.T) {
	cmd := newRootCmd()

	// Verify that all skip commands use full command paths, not bare names.
	allSkip := [][]string{
		{"login"},
		{"logout"},
		{"whoami"},
		{"status"},
		{"drive"},
		{"drive", "add"},
		{"drive", "remove"},
	}

	for _, args := range allSkip {
		sub, _, err := cmd.Find(args)
		require.NoError(t, err)

		path := sub.CommandPath()
		assert.True(t, skipConfigCommands[path],
			"CommandPath %q should be in skipConfigCommands", path)
	}

	// Verify that bare names like "add" or "remove" are NOT in the skip map
	// (protecting against future subcommand collisions).
	assert.False(t, skipConfigCommands["add"], "bare 'add' should not be in skipConfigCommands")
	assert.False(t, skipConfigCommands["remove"], "bare 'remove' should not be in skipConfigCommands")
}

// --- loadConfig tests ---

func TestLoadConfig_ValidTOML(t *testing.T) {
	oldCfg := resolvedCfg
	oldConfigPath := flagConfigPath

	t.Cleanup(func() {
		resolvedCfg = oldCfg
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
	flagConfigPath = cfgFile

	err = loadConfig(cmd)
	require.NoError(t, err)
	require.NotNil(t, resolvedCfg)

	assert.Equal(t, "personal:test@example.com", resolvedCfg.CanonicalID)
}

func TestLoadConfig_MissingFile_ZeroConfig(t *testing.T) {
	oldCfg := resolvedCfg
	oldConfigPath := flagConfigPath

	t.Cleanup(func() {
		resolvedCfg = oldCfg
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

	// ls will fail (no token), but PersistentPreRunE should succeed and populate resolvedCfg.
	_ = cmd.Execute()

	require.NotNil(t, resolvedCfg)
	assert.Equal(t, "personal:zeroconfig@example.com", resolvedCfg.CanonicalID)
}
