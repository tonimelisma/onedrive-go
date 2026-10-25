package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/perf"
)

// syncSchemaFields are the R-6.6.13 fields a classified sync result line
// carries, as engine_log_fields.go emits them.
func syncSchemaFields() []any {
	return []any{
		slog.String("run_id", "run-1234"),
		slog.Int64("action_id", 42),
		slog.String("condition_key", "upload_quota"),
		slog.String("failure_class", "actionable"),
		slog.String("log_owner", "sync"),
		slog.String("drive_id", "b!drive1234"),
		slog.String("action_type", "upload"),
		slog.String("path", "Documents/report.pdf"),
		slog.String("scope_key", "quota:own"),
		slog.Int("http_status", 507),
	}
}

func dualLoggerForTest(t *testing.T, flags CLIFlags) (*slog.Logger, *bytes.Buffer, string) {
	t.Helper()

	logPath := filepath.Join(t.TempDir(), "onedrive-go.log")
	var console bytes.Buffer
	logger, closer := buildLoggerDualWithStatusWriter(
		&config.ResolvedDrive{LoggingConfig: config.LoggingConfig{LogFile: logPath}}, flags, &console)
	require.NotNil(t, closer, "the log file must open")
	t.Cleanup(func() { assert.NoError(t, closer.Close()) })

	return logger, &console, logPath
}

func readLogFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	require.NoError(t, err)

	return string(data)
}

func logFileRecord(t *testing.T, contents, msg string) map[string]any {
	t.Helper()

	for _, line := range strings.Split(strings.TrimSpace(contents), "\n") {
		var record map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &record), "log file lines are JSON: %s", line)
		if record["msg"] == msg {
			return record
		}
	}
	require.Failf(t, "record not in log file", "%q not found in %s", msg, contents)

	return nil
}

func logPerfSummary(t *testing.T, logger *slog.Logger) {
	t.Helper()

	session, _ := perf.NewSession(t.Context(), logger, "command", "onedrive-go drive list", 0, nil)
	session.Collector().RecordHTTPRequest(200, 0, nil)
	session.Complete(nil)
}

// Validates: R-6.6.2, R-6.6.3, R-6.6.14
func TestCommandLogger_PerfSummaryInLogFileNotDefaultConsole(t *testing.T) {
	t.Parallel()

	logger, console, logPath := dualLoggerForTest(t, CLIFlags{})

	logPerfSummary(t, logger)

	assert.NotContains(t, console.String(), "performance summary",
		"the default console shows only what a person needs now")
	record := logFileRecord(t, readLogFile(t, logPath), "performance summary")
	assert.Equal(t, "INFO", record["level"], "R-6.6.14: the summary stays INFO in the log file")
	assert.Equal(t, "onedrive-go drive list", record["perf_name"])
}

// Validates: R-6.6.2, R-6.6.14
func TestCommandLogger_PerfSummaryOnConsoleWithVerboseOrDebug(t *testing.T) {
	t.Parallel()

	for _, flags := range []CLIFlags{{Verbose: true}, {Debug: true}} {
		logger, console, logPath := dualLoggerForTest(t, flags)

		logPerfSummary(t, logger)

		assert.Contains(t, console.String(), `msg="performance summary"`, "flags %+v", flags)
		assert.Contains(t, readLogFile(t, logPath), `"msg":"performance summary"`, "flags %+v", flags)
	}
}

// Validates: R-6.6.2, R-6.6.14
func TestCommandLogger_QuietConsoleDropsPerfSummaryButLogFileKeepsIt(t *testing.T) {
	t.Parallel()

	logger, console, logPath := dualLoggerForTest(t, CLIFlags{Quiet: true})

	logPerfSummary(t, logger)

	assert.Empty(t, console.String())
	assert.Contains(t, readLogFile(t, logPath), `"msg":"performance summary"`)
}

// Validates: R-6.6.13, R-6.6.21
func TestCommandLogger_SyncSchemaFieldsOnlyInLogFileAndVerboseConsole(t *testing.T) {
	t.Parallel()

	logger, console, logPath := dualLoggerForTest(t, CLIFlags{})
	logger.Warn("Couldn't upload report.pdf: OneDrive is full", syncSchemaFields()...)

	assert.Equal(t,
		"Warning: Couldn't upload report.pdf: OneDrive is full (path=Documents/report.pdf)\n",
		console.String(), "the default console keeps the path and drops the schema identifiers")

	record := logFileRecord(t, readLogFile(t, logPath), "Couldn't upload report.pdf: OneDrive is full")
	for _, key := range []string{
		"run_id", "action_id", "condition_key", "failure_class", "log_owner",
		"drive_id", "action_type", "path", "scope_key", "http_status",
	} {
		assert.Contains(t, record, key, "R-6.6.13: the log file keeps %s", key)
	}

	verboseLogger, verboseConsole, _ := dualLoggerForTest(t, CLIFlags{Verbose: true})
	verboseLogger.Warn("Couldn't upload report.pdf: OneDrive is full", syncSchemaFields()...)
	for _, field := range []string{"run_id=run-1234", "condition_key=upload_quota", "drive_id=b!drive1234", "http_status=507"} {
		assert.Contains(t, verboseConsole.String(), field)
	}
}

// Validates: R-6.6.13, R-6.6.21
func TestConsoleLogHandler_DefaultStylesDropSchemaFieldsFromWithAttrsAndGroups(t *testing.T) {
	t.Parallel()

	for _, style := range []consoleLogStyle{consoleLogHuman, consoleLogJournal} {
		var buf bytes.Buffer
		logger := slog.New(newConsoleLogHandler(&buf, slog.LevelWarn, style)).
			With(slog.String("run_id", "run-1"), slog.String("drive", "Personal")).
			WithGroup("result")

		logger.Warn("Couldn't download a.txt", slog.String("failure_class", "actionable"), slog.String("path", "a.txt"))

		line := buf.String()
		assert.NotContains(t, line, "run_id", "style %d", style)
		assert.NotContains(t, line, "failure_class", "style %d", style)
		assert.Contains(t, line, "drive=Personal", "style %d: other details stay", style)
		assert.Contains(t, line, "result.path=a.txt", "style %d", style)
	}
}

// Validates: R-6.6.13, R-6.6.21
func TestAutoConsoleHandler_JournalKeepsSchemaFieldsOnlyWhenVerbose(t *testing.T) {
	t.Parallel()

	journal := newConsoleLogHandler(&bytes.Buffer{}, slog.LevelInfo, consoleLogJournal)
	assert.False(t, journal.showInternalFields, "a bare handler hides the schema fields")

	// A real $JOURNAL_STREAM cannot be faked here, so check the rule
	// autoConsoleHandler applies to the journal handler it builds.
	for _, tc := range []struct {
		level slog.Level
		show  bool
	}{
		{slog.LevelDebug, true},
		{slog.LevelInfo, true},
		{slog.LevelWarn, false},
		{slog.LevelError, false},
	} {
		assert.Equal(t, tc.show, journalShowsInternalFields(tc.level), "level %s", tc.level)
	}
}

// Validates: R-6.6.1, R-6.6.14
//
// Commands that skip the strict config load (drive, status, auth, sync, ...)
// used to keep the bootstrap logger, which has no log file, so their
// performance summary never reached the configured log file.
func TestMainWithWriters_SkipConfigCommandLogsPerfSummaryToLogFile(t *testing.T) {
	setTestDriveHome(t)

	logPath := filepath.Join(t.TempDir(), "onedrive-go.log")
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(fmt.Sprintf("log_file = %q\n", logPath)), 0o600))

	var stdout, stderr bytes.Buffer
	code := mainWithWriters([]string{"--config", cfgPath, "drive", "list"}, &stdout, &stderr)
	require.Equal(t, 0, code, "stderr: %s", stderr.String())

	assert.NotContains(t, stderr.String(), "performance summary")
	record := logFileRecord(t, readLogFile(t, logPath), "performance summary")
	assert.Equal(t, "onedrive-go drive list", record["perf_name"])
}

// Validates: R-6.6.1
func TestUseLoggingConfig_SameSettingsKeepTheLogger(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "onedrive-go.log")
	var status bytes.Buffer
	cc := &CLIContext{StatusWriter: &status, Logger: slog.New(slog.DiscardHandler)}
	t.Cleanup(func() { assert.NoError(t, cc.closeCommandLogger()) })

	logging := config.LoggingConfig{LogFile: logPath, LogLevel: "bogus"}
	require.NoError(t, cc.useLoggingConfig(logging))
	first := cc.Logger
	require.NoError(t, cc.useLoggingConfig(logging))

	assert.Same(t, first, cc.Logger, "re-reading the same config must not reopen the log file")
	assert.Equal(t, 1, strings.Count(status.String(), "unknown log_level"),
		"the logging warning is printed once")
}

// Validates: R-6.6.1, R-6.6.21
//
// The records logged while the config file is still being found used to go
// through a separate flag-only logger: never into the log file, and in a
// different format from the rest of the run when log_format was set.
func TestMainWithWriters_BootstrapRecordsFollowTheCommandLogger(t *testing.T) {
	setTestDriveHome(t)

	logPath := filepath.Join(t.TempDir(), "onedrive-go.log")
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(fmt.Sprintf(
		"log_file = %q\nlog_level = \"debug\"\nlog_format = \"json\"\n", logPath)), 0o600))

	var stdout, stderr bytes.Buffer
	code := mainWithWriters([]string{"--debug", "--config", cfgPath, "drive", "list"}, &stdout, &stderr)
	require.Equal(t, 0, code, "stderr: %s", stderr.String())

	logFileRecord(t, readLogFile(t, logPath), "reading env overrides")

	consoleLines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	require.NotEmpty(t, consoleLines)
	assert.Contains(t, consoleLines[0], `"msg":"reading env overrides"`,
		"the first console record is the bootstrap one, rendered by the configured handler")
	for _, line := range consoleLines {
		assert.True(t, json.Valid([]byte(line)), "one format per stream; got %q", line)
	}
}

// Validates: R-6.6.21
func TestBootstrapLogBuffer_ReplaysOnlyWhatTheTargetAccepts(t *testing.T) {
	t.Parallel()

	buffer := newBootstrapLogBuffer()
	logger := slog.New(buffer).With(slog.String("phase", "bootstrap"))
	logger.Debug("finding config")
	logger.Warn("config path unusual")

	var out bytes.Buffer
	buffer.replayInto(t.Context(), newConsoleLogHandler(&out, slog.LevelWarn, consoleLogHuman))
	logger.Warn("after replay")

	assert.Equal(t,
		"Warning: config path unusual (phase=bootstrap)\nWarning: after replay (phase=bootstrap)\n",
		out.String())
}
