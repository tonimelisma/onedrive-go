package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// --- buildLogger tests ---

func TestBuildLogger_Default(t *testing.T) {
	flags := CLIFlags{}

	// nil config = bootstrap mode (pre-config).
	logger := buildLogger(nil, flags)

	// Default level is Warn.
	assert.True(t, logger.Handler().Enabled(t.Context(), slog.LevelWarn))
	assert.False(t, logger.Handler().Enabled(t.Context(), slog.LevelInfo))
}

func TestBuildLogger_Verbose(t *testing.T) {
	flags := CLIFlags{Verbose: true}

	logger := buildLogger(nil, flags)

	// --verbose sets Info.
	assert.True(t, logger.Handler().Enabled(t.Context(), slog.LevelInfo))
	assert.False(t, logger.Handler().Enabled(t.Context(), slog.LevelDebug))
}

func TestBuildLogger_Debug(t *testing.T) {
	flags := CLIFlags{Debug: true}

	logger := buildLogger(nil, flags)

	assert.True(t, logger.Handler().Enabled(t.Context(), slog.LevelDebug))
}

// Validates: R-6.6.2
func TestBuildLogger_ConfigDebug(t *testing.T) {
	cfg := &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogLevel: "debug"},
	}
	flags := CLIFlags{}

	logger := buildLogger(cfg, flags)

	assert.True(t, logger.Handler().Enabled(t.Context(), slog.LevelDebug))
}

func TestBuildLogger_VerboseOverrides(t *testing.T) {
	// Config says error, but --verbose should override to Info.
	cfg := &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogLevel: "error"},
	}
	flags := CLIFlags{Verbose: true}

	logger := buildLogger(cfg, flags)

	// --verbose enables Info level, but not Debug.
	assert.True(t, logger.Handler().Enabled(t.Context(), slog.LevelInfo))
	assert.False(t, logger.Handler().Enabled(t.Context(), slog.LevelDebug))
}

func TestBuildLogger_QuietOverrides(t *testing.T) {
	// --quiet sets Error level.
	flags := CLIFlags{Quiet: true}

	logger := buildLogger(nil, flags)

	// Error is enabled, but warn should not be.
	assert.True(t, logger.Handler().Enabled(t.Context(), slog.LevelError))
	assert.False(t, logger.Handler().Enabled(t.Context(), slog.LevelWarn))
}

func TestBuildLogger_DebugOverrides(t *testing.T) {
	// Config says error, but --debug should override to Debug.
	cfg := &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogLevel: "error"},
	}
	flags := CLIFlags{Debug: true}

	logger := buildLogger(cfg, flags)

	assert.True(t, logger.Handler().Enabled(t.Context(), slog.LevelDebug))
}

func TestBuildLogger_ConfigInfo(t *testing.T) {
	// Config log_level = "info" should set Info level.
	cfg := &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogLevel: "info"},
	}
	flags := CLIFlags{}

	logger := buildLogger(cfg, flags)

	assert.True(t, logger.Handler().Enabled(t.Context(), slog.LevelInfo))
	assert.False(t, logger.Handler().Enabled(t.Context(), slog.LevelDebug))
}

// --- cliContextFrom tests ---

func TestCliContextFrom_NilContext(t *testing.T) {
	ctx := t.Context()
	cc := cliContextFrom(ctx)
	assert.Nil(t, cc)
}

func TestCliContextFrom_WithCLIContext(t *testing.T) {
	expected := &CLIContext{
		Cfg:          &config.ResolvedDrive{SyncDir: "/test"},
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
		StatusWriter: os.Stderr,
	}
	ctx := context.WithValue(t.Context(), cliContextKey{}, expected)
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
	t.Setenv("XDG_DATA_HOME", t.TempDir()) // satisfy dev build guard

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
	t.Setenv("XDG_DATA_HOME", t.TempDir()) // satisfy dev build guard

	cmd := newRootCmd()

	// Auth and account management commands should pass through PersistentPreRunE
	// without error, because they have the skipConfigAnnotation (Phase 2 skipped).
	skipCmds := []string{"login", "logout", "whoami", "status"}
	for _, name := range skipCmds {
		t.Run(name, func(t *testing.T) {
			sub, _, err := cmd.Find([]string{name})
			require.NoError(t, err)

			sub.SetContext(t.Context())

			err = cmd.PersistentPreRunE(sub, nil)
			require.NoError(t, err, "%s should skip config loading", name)

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
	t.Setenv("XDG_DATA_HOME", t.TempDir()) // satisfy dev build guard

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

			sub.SetContext(t.Context())

			err = cmd.PersistentPreRunE(sub, nil)
			assert.NoError(t, err, "%s should skip config loading", tc.name)
		})
	}
}

// --- annotation-based skip config (tree-walking) ---

// TestAnnotationTreeWalk walks the entire command tree and verifies that every
// leaf command with RunE is correctly classified: data commands must NOT have
// skipConfigAnnotation, and all other commands must HAVE it. Any unclassified
// command fails the test, forcing authors to explicitly decide when adding
// new commands.
func TestAnnotationTreeWalk(t *testing.T) {
	// dataCommands lists leaf commands that require Phase 2 config loading
	// (no skipConfigAnnotation). This set is small and stable — adding a new
	// data command requires updating this list, which triggers a test failure
	// as a reminder to classify the new command.
	dataCommands := map[string]bool{
		"onedrive-go ls":                  true,
		"onedrive-go get":                 true,
		"onedrive-go put":                 true,
		"onedrive-go rm":                  true,
		"onedrive-go mkdir":               true,
		"onedrive-go stat":                true,
		"onedrive-go mv":                  true,
		"onedrive-go cp":                  true,
		"onedrive-go issues":              true,
		"onedrive-go issues resolve":      true,
		"onedrive-go issues clear":        true,
		"onedrive-go issues retry":        true,
		"onedrive-go verify":              true,
		"onedrive-go recycle-bin list":    true,
		"onedrive-go recycle-bin restore": true,
		"onedrive-go recycle-bin empty":   true,
	}

	cmd := newRootCmd()

	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		// Recurse into subcommands first.
		for _, sub := range c.Commands() {
			walk(sub)
		}

		// Only check leaf commands that have a RunE handler. Parent commands
		// without RunE (e.g., "recycle-bin") are grouping containers.
		if c.RunE == nil {
			// Parent group commands with skipConfig are still valid (e.g.,
			// "drive" group skips config so child resolution works). But we
			// don't enforce classification for them — only leaf commands matter.
			return
		}

		path := c.CommandPath()
		isData := dataCommands[path]

		if isData {
			assert.Empty(t, c.Annotations[skipConfigAnnotation],
				"data command %q must NOT have skipConfig annotation", path)
		} else {
			assert.Equal(t, "true", c.Annotations[skipConfigAnnotation],
				"non-data command %q must have skipConfig annotation (or add it to dataCommands)", path)
		}
	}

	walk(cmd)

	// Verify the dataCommands set doesn't have stale entries.
	for path := range dataCommands {
		// Split "onedrive-go ls" → ["ls"], "onedrive-go issues resolve" → ["issues", "resolve"]
		parts := strings.SplitN(path, " ", 2)
		var args []string
		if len(parts) == 2 {
			args = strings.Split(parts[1], " ")
		}

		sub, _, err := cmd.Find(args)
		require.NoError(t, err, "dataCommands entry %q not found in command tree", path)
		assert.NotNil(t, sub.RunE,
			"dataCommands entry %q has no RunE — remove it from the set", path)
	}
}

// --- defaultHTTPClient tests ---

func TestDefaultHTTPClient_HasTimeout(t *testing.T) {
	client := defaultHTTPClient(slog.Default())
	assert.Equal(t, httpClientTimeout, client.Timeout)
	assert.NotNil(t, client.Transport, "should have RetryTransport")
}

// Validates: R-6.2.10
func TestTransferHTTPClient_NoTimeout(t *testing.T) {
	client := transferHTTPClient(slog.Default())
	assert.Zero(t, client.Timeout, "client-level timeout must be zero for large transfers")
	assert.NotNil(t, client.Transport, "should have RetryTransport")

	// Unwrap RetryTransport to verify transport-level protection.
	rt, ok := client.Transport.(*retry.RetryTransport)
	require.True(t, ok, "transport should be *retry.RetryTransport")

	inner, ok := rt.Inner.(*http.Transport)
	require.True(t, ok, "inner transport should be *http.Transport")
	assert.Equal(t, transferResponseHeaderTimeout, inner.ResponseHeaderTimeout,
		"transport must have ResponseHeaderTimeout to detect stalled connections")
}

func TestSyncMetaHTTPClient_NoRetryTransport(t *testing.T) {
	client := syncMetaHTTPClient()
	assert.Equal(t, httpClientTimeout, client.Timeout)
	assert.Nil(t, client.Transport, "sync client should have no RetryTransport")
}

// Validates: R-6.2.10
func TestSyncTransferHTTPClient_NoRetryTransport(t *testing.T) {
	client := syncTransferHTTPClient()
	assert.Zero(t, client.Timeout, "client-level timeout must be zero for large transfers")

	// syncTransferHTTPClient has no RetryTransport but still needs
	// transport-level protection via transferTransport().
	inner, ok := client.Transport.(*http.Transport)
	require.True(t, ok, "transport should be *http.Transport with ResponseHeaderTimeout")
	assert.Equal(t, transferResponseHeaderTimeout, inner.ResponseHeaderTimeout,
		"transport must have ResponseHeaderTimeout to detect stalled connections")
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
	cmd.SetContext(t.Context())

	resolved, rawCfg, err := loadAndResolve(cmd, flags, config.EnvOverrides{}, logger)
	require.NoError(t, err)
	assert.NotNil(t, resolved)
	assert.NotNil(t, rawCfg)
	assert.Equal(t, "personal:test@example.com", resolved.CanonicalID.String())
}

func TestLoadAndResolve_MissingFile_NoDrives_Error(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "nonexistent.toml")

	// Override HOME so token discovery finds nothing on disk.
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_DATA_HOME", tmpDir) // satisfy dev build guard

	// No config file and no tokens: matchNoDrives returns "no accounts
	// configured" because no tokens exist on disk.
	cmd := newRootCmd()
	cmd.SetArgs([]string{"--drive", "personal:zeroconfig@example.com", "--config", cfgPath, "ls"})

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no accounts configured")
}

// --- mustCLIContext tests ---

func TestMustCLIContext_Panics(t *testing.T) {
	assert.PanicsWithValue(t,
		"BUG: CLIContext not found in context — ensure the command "+
			"does not skip config loading (no skipConfigAnnotation) or "+
			"explicitly loads config in its RunE",
		func() { mustCLIContext(t.Context()) },
	)
}

func TestMustCLIContext_Returns(t *testing.T) {
	expected := &CLIContext{
		Cfg:          &config.ResolvedDrive{SyncDir: "/must-test"},
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
		StatusWriter: os.Stderr,
	}
	ctx := context.WithValue(t.Context(), cliContextKey{}, expected)
	cc := mustCLIContext(ctx)
	assert.Equal(t, expected, cc)
	assert.Equal(t, "/must-test", cc.Cfg.SyncDir)
}

// --- CLIFlags tests ---

func TestCLIFlags_PopulatedByPersistentPreRunE(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir()) // satisfy dev build guard

	// Verify that PersistentPreRunE populates CLIFlags for auth commands.
	cmd := newRootCmd()
	cmd.SetArgs([]string{"--verbose", "status"})

	require.NoError(t, cmd.Execute())

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
	cmd.SetContext(t.Context())

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
	cmd.SetContext(t.Context())

	_, _, err = loadAndResolve(cmd, flags, config.EnvOverrides{}, logger)
	require.Error(t, err)
	// MatchDrive errors are returned unwrapped — they're user-facing messages.
	assert.Contains(t, err.Error(), "multiple drives configured")
}

func TestLoadAndResolve_NoDoubleWrapping(t *testing.T) {
	tmpDir := t.TempDir()
	cfgFile := filepath.Join(tmpDir, "config.toml")

	// Invalid TOML triggers LoadOrDefault error, which ResolveDrive wraps
	// with "loading config: ". Verify loadAndResolve doesn't double-wrap.
	err := os.WriteFile(cfgFile, []byte("{{invalid toml"), 0o600)
	require.NoError(t, err)

	flags := CLIFlags{ConfigPath: cfgFile}
	logger := buildLogger(nil, flags)

	cmd := newRootCmd()
	cmd.SetContext(t.Context())

	_, _, err = loadAndResolve(cmd, flags, config.EnvOverrides{}, logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "loading config")
	assert.NotContains(t, err.Error(), "loading config: loading config",
		"loadAndResolve must not double-wrap errors from ResolveDrive")
}

// --- buildHandler / format tests ---
// These tests are safe for t.Parallel() — no mutable globals; TTY detection
// is based on the io.Writer passed to buildHandler, not package-level state.

func TestBuildHandler_FormatText(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cfg := &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogFormat: "text"},
	}
	handler := buildHandler(&buf, cfg, &slog.HandlerOptions{})
	_, ok := handler.(*slog.TextHandler)
	assert.True(t, ok, "expected *slog.TextHandler")
}

func TestBuildHandler_FormatJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cfg := &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogFormat: "json"},
	}
	handler := buildHandler(&buf, cfg, &slog.HandlerOptions{})
	_, ok := handler.(*slog.JSONHandler)
	assert.True(t, ok, "expected *slog.JSONHandler")
}

func TestBuildHandler_FormatAutoNonTTY(t *testing.T) {
	t.Parallel()

	// bytes.Buffer has no Fd() method → isWriterTTY returns false → JSON.
	var buf bytes.Buffer
	cfg := &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogFormat: "auto"},
	}
	handler := buildHandler(&buf, cfg, &slog.HandlerOptions{})
	_, ok := handler.(*slog.JSONHandler)
	assert.True(t, ok, "expected *slog.JSONHandler for non-TTY writer")
}

func TestBuildHandler_NilConfigUsesText(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	handler := buildHandler(&buf, nil, &slog.HandlerOptions{})
	_, ok := handler.(*slog.TextHandler)
	assert.True(t, ok, "nil config should use *slog.TextHandler")
}

func TestBuildHandler_EmptyFormatUsesText(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cfg := &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogFormat: ""},
	}
	handler := buildHandler(&buf, cfg, &slog.HandlerOptions{})
	_, ok := handler.(*slog.TextHandler)
	assert.True(t, ok, "empty format should use *slog.TextHandler")
}

func TestBuildHandler_UnknownFormatWarnsAndFallsBack(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cfg := &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogFormat: "xml"},
	}
	handler := buildHandler(&buf, cfg, &slog.HandlerOptions{})

	// Should fall back to text handler.
	_, ok := handler.(*slog.TextHandler)
	assert.True(t, ok, "unknown format should fall back to *slog.TextHandler")

	// Warning should be written to the same writer (not hardcoded stderr).
	assert.Contains(t, buf.String(), `unknown log format "xml"`)
}

// Validates: R-4.7.2, R-6.6.4
func TestBuildHandler_JSONOutputFormat(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cfg := &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{
			LogFormat: "json",
			LogLevel:  "debug",
		},
	}
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	handler := buildHandler(&buf, cfg, opts)
	logger := slog.New(handler)

	logger.Info("test message", slog.String("key", "value"))

	var record map[string]any
	err := json.Unmarshal(buf.Bytes(), &record)
	require.NoError(t, err, "output should be valid JSON: %s", buf.String())
	assert.Equal(t, "test message", record["msg"])
	assert.Equal(t, "INFO", record["level"])
	assert.Equal(t, "value", record["key"])
	assert.Contains(t, record, "time")
}

func TestBuildHandler_JSONFormatWithDebugLevel(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cfg := &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{
			LogFormat: "json",
			LogLevel:  "debug",
		},
	}
	level := resolveLogLevel(cfg, CLIFlags{})
	opts := &slog.HandlerOptions{Level: level}
	handler := buildHandler(&buf, cfg, opts)

	// Verify correct handler type.
	_, ok := handler.(*slog.JSONHandler)
	assert.True(t, ok, "expected *slog.JSONHandler")

	// Verify the level was correctly resolved.
	assert.Equal(t, slog.LevelDebug, level)

	// Verify debug messages are emitted.
	logger := slog.New(handler)
	logger.Debug("debug msg")
	assert.Contains(t, buf.String(), "debug msg")
}

func TestIsWriterTTY_Buffer(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	assert.False(t, isWriterTTY(&buf), "bytes.Buffer should not be a TTY")
}

func TestIsWriterTTY_File(t *testing.T) {
	t.Parallel()

	// A regular file is not a TTY.
	f, err := os.CreateTemp(t.TempDir(), "test")
	require.NoError(t, err)
	defer f.Close()

	assert.False(t, isWriterTTY(f), "regular file should not be a TTY")
}

func TestBuildLogger_UnknownLogLevel(t *testing.T) {
	cfg := &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{LogLevel: "bogus"},
	}
	flags := CLIFlags{}

	// Captures stderr warning for unknown log level.
	logger := buildLogger(cfg, flags)

	// Should fall back to warn level.
	assert.True(t, logger.Handler().Enabled(t.Context(), slog.LevelWarn))
	assert.False(t, logger.Handler().Enabled(t.Context(), slog.LevelInfo))
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
				logger.Handler().Enabled(t.Context(), slog.LevelInfo),
				"Info enabled mismatch for log_level=%q", tc.logLevel)
			assert.Equal(t, tc.wantDebug,
				logger.Handler().Enabled(t.Context(), slog.LevelDebug),
				"Debug enabled mismatch for log_level=%q", tc.logLevel)
		})
	}
}

// --- SingleDrive tests ---

func TestSingleDrive_Empty(t *testing.T) {
	flags := CLIFlags{}
	got, err := flags.SingleDrive()
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestSingleDrive_One(t *testing.T) {
	flags := CLIFlags{Drive: []string{"personal:user@example.com"}}
	got, err := flags.SingleDrive()
	require.NoError(t, err)
	assert.Equal(t, "personal:user@example.com", got)
}

func TestSingleDrive_Multiple_ReturnsError(t *testing.T) {
	flags := CLIFlags{Drive: []string{"drive1", "drive2"}}
	_, err := flags.SingleDrive()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple --drive values")
}

// --- errVerifyMismatch tests ---

func TestErrVerifyMismatch_IsSentinel(t *testing.T) {
	// Verify the sentinel error is usable with errors.Is.
	wrapped := fmt.Errorf("wrapped: %w", errVerifyMismatch)
	assert.ErrorIs(t, wrapped, errVerifyMismatch)
}

// --- Command structure tests ---

// Validates: R-1.2, R-6.2.8
func TestNewGetCmd_Structure(t *testing.T) {
	t.Parallel()

	cmd := newGetCmd()
	assert.Equal(t, "get <remote-path> [local-path]", cmd.Use)
}

// Validates: R-1.3
func TestNewPutCmd_Structure(t *testing.T) {
	t.Parallel()

	cmd := newPutCmd()
	assert.Equal(t, "put <local-path> [remote-path]", cmd.Use)
}

// Validates: R-1.4
func TestNewRmCmd_Structure(t *testing.T) {
	t.Parallel()

	cmd := newRmCmd()
	assert.Equal(t, "rm <path>", cmd.Use)
	assert.NotNil(t, cmd.Flags().Lookup("recursive"))
	assert.NotNil(t, cmd.Flags().Lookup("permanent"))
}

// Validates: R-1.5
func TestNewMkdirCmd_Structure(t *testing.T) {
	t.Parallel()

	cmd := newMkdirCmd()
	assert.Equal(t, "mkdir <path>", cmd.Use)
}

// --- multiHandler tests ---

func TestMultiHandler_Enabled_ORSemantics(t *testing.T) {
	t.Parallel()

	var buf1, buf2 bytes.Buffer
	h1 := slog.NewTextHandler(&buf1, &slog.HandlerOptions{Level: slog.LevelError})
	h2 := slog.NewTextHandler(&buf2, &slog.HandlerOptions{Level: slog.LevelDebug})

	mh := &multiHandler{handlers: []slog.Handler{h1, h2}}

	// Debug is enabled because h2 allows it (OR semantics).
	assert.True(t, mh.Enabled(t.Context(), slog.LevelDebug),
		"should be enabled when any handler accepts the level")
}

func TestMultiHandler_Enabled_AllDisabled(t *testing.T) {
	t.Parallel()

	var buf1, buf2 bytes.Buffer
	h1 := slog.NewTextHandler(&buf1, &slog.HandlerOptions{Level: slog.LevelError})
	h2 := slog.NewTextHandler(&buf2, &slog.HandlerOptions{Level: slog.LevelError})

	mh := &multiHandler{handlers: []slog.Handler{h1, h2}}

	assert.False(t, mh.Enabled(t.Context(), slog.LevelDebug),
		"should be disabled when no handler accepts the level")
}

func TestMultiHandler_Handle_PerHandlerFiltering(t *testing.T) {
	t.Parallel()

	var buf1, buf2 bytes.Buffer
	h1 := slog.NewTextHandler(&buf1, &slog.HandlerOptions{Level: slog.LevelError})
	h2 := slog.NewTextHandler(&buf2, &slog.HandlerOptions{Level: slog.LevelDebug})

	mh := &multiHandler{handlers: []slog.Handler{h1, h2}}
	logger := slog.New(mh)

	logger.Debug("debug msg")

	// h1 (Error level) should not receive the debug message.
	assert.Empty(t, buf1.String(), "error-level handler should not receive debug messages")
	// h2 (Debug level) should receive it.
	assert.Contains(t, buf2.String(), "debug msg", "debug-level handler should receive debug messages")
}

func TestMultiHandler_Handle_BothReceive(t *testing.T) {
	t.Parallel()

	var buf1, buf2 bytes.Buffer
	h1 := slog.NewTextHandler(&buf1, &slog.HandlerOptions{Level: slog.LevelInfo})
	h2 := slog.NewJSONHandler(&buf2, &slog.HandlerOptions{Level: slog.LevelInfo})

	mh := &multiHandler{handlers: []slog.Handler{h1, h2}}
	logger := slog.New(mh)

	logger.Info("test message", slog.String("key", "value"))

	assert.Contains(t, buf1.String(), "test message", "text handler should receive the message")
	assert.Contains(t, buf2.String(), "test message", "json handler should receive the message")
}

func TestMultiHandler_WithAttrs(t *testing.T) {
	t.Parallel()

	var buf1, buf2 bytes.Buffer
	h1 := slog.NewTextHandler(&buf1, &slog.HandlerOptions{Level: slog.LevelInfo})
	h2 := slog.NewJSONHandler(&buf2, &slog.HandlerOptions{Level: slog.LevelInfo})

	mh := &multiHandler{handlers: []slog.Handler{h1, h2}}
	mh2 := mh.WithAttrs([]slog.Attr{slog.String("component", "test")})
	logger := slog.New(mh2)

	logger.Info("attrs test")

	assert.Contains(t, buf1.String(), "component", "attrs should propagate to text handler")
	assert.Contains(t, buf2.String(), "component", "attrs should propagate to json handler")
}

func TestMultiHandler_WithGroup(t *testing.T) {
	t.Parallel()

	var buf1, buf2 bytes.Buffer
	h1 := slog.NewTextHandler(&buf1, &slog.HandlerOptions{Level: slog.LevelInfo})
	h2 := slog.NewJSONHandler(&buf2, &slog.HandlerOptions{Level: slog.LevelInfo})

	mh := &multiHandler{handlers: []slog.Handler{h1, h2}}
	mh2 := mh.WithGroup("mygroup")
	logger := slog.New(mh2)

	logger.Info("group test", slog.String("key", "val"))

	assert.Contains(t, buf1.String(), "mygroup", "group should propagate to text handler")
	assert.Contains(t, buf2.String(), "mygroup", "group should propagate to json handler")
}

// --- buildLogger dual-output tests ---

func TestBuildLoggerDual_NoLogFile(t *testing.T) {
	t.Parallel()

	cfg := &config.ResolvedDrive{}
	flags := CLIFlags{}
	logger, closer := buildLoggerDual(cfg, flags)

	assert.NotNil(t, logger)
	assert.Nil(t, closer, "closer should be nil when no log file is set")
}

// Validates: R-4.7.1, R-6.6.1
func TestBuildLoggerDual_WithLogFile(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "test.log")
	cfg := &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{
			LogFile:  logPath,
			LogLevel: "debug",
		},
	}
	flags := CLIFlags{}

	logger, closer := buildLoggerDual(cfg, flags)
	require.NotNil(t, logger)
	require.NotNil(t, closer, "closer should be non-nil when log file is set")

	logger.Info("test log message", slog.String("key", "value"))
	require.NoError(t, closer.Close())

	data, err := os.ReadFile(logPath) //nolint:gosec // Test log path is created in t.TempDir and controlled by the test.
	require.NoError(t, err)
	assert.Contains(t, string(data), "test log message")
	assert.Contains(t, string(data), "key")
}

// Validates: R-4.7.2, R-6.6.4
func TestBuildLoggerDual_FileGetsJSON(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "json.log")
	cfg := &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{
			LogFile:  logPath,
			LogLevel: "info",
		},
	}
	flags := CLIFlags{}

	logger, closer := buildLoggerDual(cfg, flags)
	require.NotNil(t, closer)

	logger.Info("json check")
	require.NoError(t, closer.Close())

	data, err := os.ReadFile(logPath) //nolint:gosec // Test log path is created in t.TempDir and controlled by the test.
	require.NoError(t, err)

	// File output should be JSON.
	var record map[string]any
	require.NoError(t, json.Unmarshal(data, &record), "file output should be valid JSON: %s", string(data))
	assert.Equal(t, "json check", record["msg"])
}

// Validates: R-6.6.3
func TestBuildLoggerDual_IndependentLevels(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "levels.log")
	cfg := &config.ResolvedDrive{
		LoggingConfig: config.LoggingConfig{
			LogFile:  logPath,
			LogLevel: "debug",
		},
	}
	// CLI flag sets console to error-only, but file should still get debug.
	flags := CLIFlags{Quiet: true}

	logger, closer := buildLoggerDual(cfg, flags)
	require.NotNil(t, closer)

	logger.Debug("debug for file only")
	require.NoError(t, closer.Close())

	data, err := os.ReadFile(logPath) //nolint:gosec // Test log path is created in t.TempDir and controlled by the test.
	require.NoError(t, err)
	assert.Contains(t, string(data), "debug for file only",
		"file handler should receive debug messages regardless of CLI flags")
}
