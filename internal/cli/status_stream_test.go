package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// failingTerminal is a status writer isWriterTTY takes for a terminal, since
// its descriptor is a pseudo-terminal's, and whose every write fails, as a
// terminal that hung up does.
type failingTerminal struct {
	terminal *os.File
}

func (w failingTerminal) Fd() uintptr { return w.terminal.Fd() }

func (failingTerminal) Write([]byte) (int, error) {
	return 0, errors.New("terminal hung up")
}

// openFailingTerminal opens a pseudo-terminal for a failingTerminal. It
// skips, saying why, where the platform's pseudo-terminal is not a terminal
// to isatty.
func openFailingTerminal(t *testing.T) failingTerminal {
	t.Helper()

	terminal, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	require.NoError(t, err, "open a pseudo-terminal")
	t.Cleanup(func() { assert.NoError(t, terminal.Close()) })

	writer := failingTerminal{terminal: terminal}
	if !isWriterTTY(writer) {
		t.Skip("this platform's pseudo-terminal is not a terminal to isatty")
	}

	return writer
}

// A warning the status stream refuses is recorded, like a status line, so
// main can fail the command.
//
// Validates: R-6.6.31
func TestWarnings_AFailedWriteIsRecorded(t *testing.T) {
	t.Parallel()

	cc := &CLIContext{StatusWriter: errWriter{}}
	printSharePointGapNotes(cc, sharePointGaps{
		FailedSiteSearches: []failedSiteSearch{{Account: "user@example.com", Reason: gapReasonDenied}},
	})

	require.ErrorContains(t, cc.StatusError(), "write failed")
}

// The command logger's warnings about the config (an unknown log_format
// here) are command warnings too: one the status stream refuses is recorded.
//
// Validates: R-6.6.1, R-6.6.31
func TestUseLoggingConfig_AWarningItCannotWriteIsRecorded(t *testing.T) {
	t.Parallel()

	cc := &CLIContext{StatusWriter: errWriter{}, Logger: slog.New(slog.DiscardHandler)}
	require.NoError(t, cc.useLoggingConfig(config.LoggingConfig{LogFormat: "xml"}))

	require.ErrorContains(t, cc.StatusError(), "write failed")
}

// Validates: R-6.6.29, R-6.6.31
func TestProgressf_RecordsAFailedWrite(t *testing.T) {
	t.Parallel()

	cc := &CLIContext{StatusWriter: openFailingTerminal(t)}
	cc.Progressf("Uploading: %s / %s", "10 MB", "50 MB")

	require.ErrorContains(t, cc.StatusError(), "terminal hung up")
}

// Validates: R-6.6.29, R-6.6.31
func TestProgressDone_RecordsAFailedWrite(t *testing.T) {
	t.Parallel()

	cc := &CLIContext{StatusWriter: errWriter{}, progressShown: true}
	cc.ProgressDone()

	require.ErrorContains(t, cc.StatusError(), "write failed")
	assert.False(t, cc.progressShown, "the line counts as cleared: the next write starts a new one")
}

// resumeTestConfig writes a config with a log file and one drive that is not
// paused, so `resume` succeeds after one status line, "No paused drives
// found".
func resumeTestConfig(t *testing.T) (cfgPath, logPath string) {
	t.Helper()

	dir := t.TempDir()
	logPath = filepath.Join(dir, "onedrive-go.log")
	cfgPath = filepath.Join(dir, "config.toml")
	content := fmt.Sprintf("log_file = %q\n\n", logPath) +
		`["personal:user@example.com"]` + "\n" +
		fmt.Sprintf("sync_dir = %q\n", filepath.Join(dir, "OneDrive"))
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o600))

	return cfgPath, logPath
}

// A command that did its work but could not write to its status stream ends
// with exit 1, as the GNU tools do when standard error fails; the failure is
// in the log file, once. With a working status stream the same command exits
// 0, so the stream alone made the difference.
//
// Validates: R-6.6.31
func TestMainWithWriters_AStatusStreamThatFailsAWriteFailsAWorkingCommand(t *testing.T) {
	setTestDriveHome(t)

	cfgPath, logPath := resumeTestConfig(t)
	var stdout, stderr bytes.Buffer
	require.Equal(t, exitSuccess, mainWithWriters([]string{"--config", cfgPath, "resume"}, &stdout, &stderr))
	require.Equal(t, "No paused drives found\n", stderr.String())
	assert.NotContains(t, readLogFile(t, logPath), statusStreamFailedMessage)

	cfgPath, logPath = resumeTestConfig(t)
	exitCode := mainWithWriters([]string{"--config", cfgPath, "resume"}, &stdout, errWriter{})

	assert.Equal(t, exitFailure, exitCode)
	contents := readLogFile(t, logPath)
	assert.Equal(t, 1, strings.Count(contents, `"msg":"`+statusStreamFailedMessage+`"`), contents)
	record := logFileRecord(t, contents, statusStreamFailedMessage)
	assert.Equal(t, "WARN", record["level"])
	assert.Contains(t, record["error"], "write failed")
}

// A command that failed for its own reason keeps its exit status and its
// message when the status stream fails too.
//
// Validates: R-6.6.31
func TestMainWithWriters_AFailedCommandKeepsItsStatusWhenTheStatusStreamFails(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("transfer_worker = 8\n"), 0o600))

	var stdout bytes.Buffer
	assert.Equal(t, exitConfigInvalid, mainWithWriters([]string{"--config", cfgPath, "ls", "/"}, &stdout, errWriter{}))
	assert.Empty(t, stdout.String())
}

// A failed command whose stderr refuses main's final error keeps its own
// exit status, and the refusal is in the log file once at warn level: the
// final error is written while the log file is still open.
//
// Validates: R-6.6.31
func TestMainWithWriters_ARefusedFinalErrorIsRecordedInTheLogFile(t *testing.T) {
	setTestDriveHome(t)

	dir := t.TempDir()
	logPath := filepath.Join(dir, "onedrive-go.log")
	cfgPath := filepath.Join(dir, "config.toml")
	content := fmt.Sprintf("log_file = %q\n\n", logPath) +
		`["personal:user@example.com"]` + "\n" +
		fmt.Sprintf("sync_dir = %q\n", filepath.Join(dir, "OneDrive"))
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o600))

	var stdout bytes.Buffer
	exitCode := mainWithWriters([]string{"--config", cfgPath, "ls", "/"}, &stdout, errWriter{})

	assert.Equal(t, exitSignInRequired, exitCode, "the command's own status")
	contents := readLogFile(t, logPath)
	assert.Equal(t, 1, strings.Count(contents, `"msg":"`+statusStreamFailedMessage+`"`), contents)
	record := logFileRecord(t, contents, statusStreamFailedMessage)
	assert.Equal(t, "WARN", record["level"])
	assert.Contains(t, record["error"], "write failed")
}

// runWatchThatWarnsAtStart runs a watch on ctx whose orchestrator delivers
// the warning that a drive did not start, on a status stream that refuses
// every write.
func runWatchThatWarnsAtStart(t *testing.T, ctx context.Context, logger *slog.Logger) error {
	t.Helper()

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:watch@example.com")
	syncDir := t.TempDir()
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, syncDir))
	holder := loadSyncTestHolder(t, cfgPath)

	return runSyncDaemonWithFactory(
		ctx,
		holder,
		nil,
		syncdomain.SyncBidirectional,
		syncengine.WatchOptions{},
		logger,
		errWriter{},
		"/tmp/control.sock",
		nil,
		nil,
		func(cfg *multisync.OrchestratorConfig) syncDaemonOrchestrator {
			cfg.StartWarning(multisync.StartupWarning{
				Summary: multisync.StartupSelectionSummary{
					Results: []multisync.MountStartupResult{{
						Identity: testStandaloneMountIdentity(cid),
						Status:   multisync.MountStartupFatal,
						Err:      errors.New("sync folder missing"),
					}},
				},
			})
			return &testSyncDaemonOrchestrator{}
		},
	)
}

// The warning that a drive did not start reaches the watch through the
// orchestrator's hook; one the status stream refuses is recorded in the
// command context like a status line, so main fails a watch that then stops
// cleanly.
//
// Validates: R-6.6.31
func TestRunSyncDaemonWithFactory_AStartWarningThatCannotBeWrittenIsRecorded(t *testing.T) {
	setTestDriveHome(t)

	ctx := newSyncTestContext(t.Context(), "", errWriter{})
	require.NoError(t, runWatchThatWarnsAtStart(t, ctx, slog.New(slog.DiscardHandler)))

	require.ErrorContains(t, cliContextFrom(ctx).StatusError(), "write failed")
}

// Run without a command context, the watch gives a start warning the status
// stream refuses to its logger, the log file's half included, at warn level.
//
// Validates: R-6.6.1
func TestRunSyncDaemonWithFactory_AStartWarningThatCannotBeWrittenIsLoggedWithoutACommandContext(t *testing.T) {
	setTestDriveHome(t)

	var logged bytes.Buffer
	require.NoError(t, runWatchThatWarnsAtStart(t, t.Context(), slog.New(slog.NewJSONHandler(&logged, nil))))

	contents := logged.String()
	assert.Equal(t, 1, strings.Count(contents, `"msg":"`+startWarningUnwrittenMessage+`"`), contents)
	record := logFileRecord(t, contents, startWarningUnwrittenMessage)
	assert.Equal(t, "WARN", record["level"])
	assert.Contains(t, record["error"], "write failed")
}
