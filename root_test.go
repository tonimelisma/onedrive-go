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

// --- buildLogger tests ---

func TestBuildLogger_Default(t *testing.T) {
	oldCfg := resolvedCfg
	oldVerbose := flagVerbose
	oldQuiet := flagQuiet

	t.Cleanup(func() {
		resolvedCfg = oldCfg
		flagVerbose = oldVerbose
		flagQuiet = oldQuiet
	})

	resolvedCfg = nil
	flagVerbose = false
	flagQuiet = false

	logger := buildLogger()

	// Default level is Info: Info enabled, Debug not.
	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelInfo))
	assert.False(t, logger.Handler().Enabled(context.Background(), slog.LevelDebug))
}

func TestBuildLogger_ConfigDebug(t *testing.T) {
	oldCfg := resolvedCfg
	oldVerbose := flagVerbose
	oldQuiet := flagQuiet

	t.Cleanup(func() {
		resolvedCfg = oldCfg
		flagVerbose = oldVerbose
		flagQuiet = oldQuiet
	})

	resolvedCfg = &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogLevel: "debug"},
	}
	flagVerbose = false
	flagQuiet = false

	logger := buildLogger()

	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelDebug))
}

func TestBuildLogger_VerboseOverrides(t *testing.T) {
	oldCfg := resolvedCfg
	oldVerbose := flagVerbose
	oldQuiet := flagQuiet

	t.Cleanup(func() {
		resolvedCfg = oldCfg
		flagVerbose = oldVerbose
		flagQuiet = oldQuiet
	})

	// Config says warn, but --verbose should override to debug.
	resolvedCfg = &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogLevel: "warn"},
	}
	flagVerbose = true
	flagQuiet = false

	logger := buildLogger()

	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelDebug))
}

func TestBuildLogger_QuietOverrides(t *testing.T) {
	oldCfg := resolvedCfg
	oldVerbose := flagVerbose
	oldQuiet := flagQuiet

	t.Cleanup(func() {
		resolvedCfg = oldCfg
		flagVerbose = oldVerbose
		flagQuiet = oldQuiet
	})

	// Even with verbose set, quiet wins (it is checked last).
	resolvedCfg = nil
	flagVerbose = true
	flagQuiet = true

	logger := buildLogger()

	// Error is enabled, but warn should not be.
	assert.True(t, logger.Handler().Enabled(context.Background(), slog.LevelError))
	assert.False(t, logger.Handler().Enabled(context.Background(), slog.LevelWarn))
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

	expectedFlags := []string{"config", "account", "drive", "json", "verbose", "quiet"}
	for _, name := range expectedFlags {
		flag := cmd.PersistentFlags().Lookup(name)
		assert.NotNil(t, flag, "expected persistent flag %q not found", name)
	}
}

func TestNewRootCmd_AuthSkipsConfig(t *testing.T) {
	cmd := newRootCmd()

	// Auth and account management commands should pass through PersistentPreRunE
	// without error, because they skip the four-layer config resolution.
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

	// drive add and drive remove should skip config loading via PersistentPreRunE.
	driveSubCmds := []string{"add", "remove"}
	for _, name := range driveSubCmds {
		t.Run(name, func(t *testing.T) {
			sub, _, err := cmd.Find([]string{"drive", name})
			require.NoError(t, err)

			err = cmd.PersistentPreRunE(sub, nil)
			assert.NoError(t, err, "drive %s should skip config loading", name)
		})
	}
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
	flagConfigPath = filepath.Join(tmpDir, "nonexistent.toml")

	// Zero-config mode: no config file, but --drive is set with a canonical ID.
	// The resolver allows this because the selector contains ":" (canonical format),
	// even when no drives are configured in the file.
	// We use Execute with the ls subcommand so Cobra properly merges persistent
	// flags and marks --drive as changed -- matching real CLI invocation.
	cmd := newRootCmd()
	cmd.SetArgs([]string{"--drive", "personal:zeroconfig@example.com", "--config", flagConfigPath, "ls"})

	// ls will fail (no token), but PersistentPreRunE should succeed and populate resolvedCfg.
	_ = cmd.Execute()

	require.NotNil(t, resolvedCfg)
	assert.Equal(t, "personal:zeroconfig@example.com", resolvedCfg.CanonicalID)
}
