package cli

import (
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/loglevel"
)

// Validates: R-6.6.21
func TestConsoleLogHandler_JournalLinesCarryPriorityAndNoTimestamp(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(newConsoleLogHandler(&buf, slog.LevelInfo, consoleLogJournal)).With("drive", "personal")

	logger.Info("watch mode starting")
	logger.Warn("remote watch poll failed", slog.String("error", "timeout"))
	logger.Error("store unavailable")
	logger.Debug("hidden")

	assert.Equal(t,
		"<6>watch mode starting drive=personal\n"+
			"<4>remote watch poll failed drive=personal error=timeout\n"+
			"<3>store unavailable drive=personal\n",
		buf.String())
}

// Validates: R-6.6.21
func TestConsoleLogHandler_HumanStyleQuotesAndGroups(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(newConsoleLogHandler(&buf, slog.LevelWarn, consoleLogHuman)).WithGroup("scope")

	logger.Warn("upload blocked", slog.String("path", "My Files/a.txt"), slog.Int("count", 2))

	assert.Equal(t, "Warning: upload blocked (scope.path=\"My Files/a.txt\", scope.count=2)\n", buf.String())
}

// Validates: R-6.6.23
func TestUnauthorizedAuthReason_DistinguishesExpiredLogin(t *testing.T) {
	t.Parallel()

	expired := fmt.Errorf("graph: obtaining token: %w: %w", graph.ErrLoginExpired, graph.ErrUnauthorized)
	rejected := fmt.Errorf("GET /me: %w", graph.ErrUnauthorized)

	assert.Equal(t, authstate.ReasonSavedLoginExpired, unauthorizedAuthReason(expired))
	assert.Equal(t, authstate.ReasonSyncAuthRejected, unauthorizedAuthReason(rejected))
	assert.Contains(t, authErrorMessage(expired), "no longer accepts the saved login")
}

// Validates: R-6.6.21
func TestConsoleLogHandler_HumanStyleHidesSupersedeDiagnostics(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(newConsoleLogHandler(&buf, slog.LevelWarn, consoleLogHuman))

	logger.Warn("A planned change keeps going stale before it can run; sync will keep retrying it",
		slog.String("path", "playground/movedst/inner"),
		slog.String("reason", "it no longer matches what sync planned"),
		slog.String("action_type", "remote_move"),
		slog.Int("supersede_streak", 3),
		slog.String("error_detail", "remote move remote item C763212F!s1eaa moved to playground/movedst/inner"),
	)

	assert.Equal(t,
		"Warning: A planned change keeps going stale before it can run; sync will keep retrying it "+
			"(path=playground/movedst/inner, reason=\"it no longer matches what sync planned\")\n",
		buf.String())
}

// Regression: the watch console showed "Can't check OneDrive for changes" but
// never its recovery (logged at INFO, below the default WARN console), and the
// warning carried the whole error chain with the delta URL and its cursor.
//
// Validates: R-6.6.30
func TestConsoleLogHandler_DefaultConsoleShowsOutageAndItsRecoveryPlainly(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(autoConsoleHandler(&buf, &slog.HandlerOptions{Level: resolveConsoleLogLevel(CLIFlags{})}))

	dns := &net.DNSError{Err: "connect: network is unreachable", Name: "login.microsoftonline.com", Server: "192.0.2.53:53"}
	dial := &net.OpError{Op: "dial", Net: "tcp", Err: dns}
	post := &url.Error{Op: "Post", URL: "https://login.microsoftonline.com/common/oauth2/v2.0/token", Err: dial}
	chain := fmt.Errorf("sync: fetching delta page 0: graph: GET /drives/d/root/delta?token=SECRET-CURSOR: "+
		"obtaining token: refreshing access token: %w", post)

	logger.Warn("Can't check OneDrive for changes; retrying",
		slog.Any("error", chain), slog.String("retry_in", "5 s"), slog.Int("consecutive_errors", 1))
	logger.Info("an operational record the default console keeps out of the way")
	logger.Log(t.Context(), loglevel.Notice, "Checking OneDrive for changes works again")

	assert.Equal(t,
		"Warning: Can't check OneDrive for changes; retrying "+
			"(can't reach OneDrive (could not look up login.microsoftonline.com), retry_in=\"5 s\")\n"+
			"Checking OneDrive for changes works again\n",
		buf.String())
	assert.NotContains(t, buf.String(), "SECRET-CURSOR", "the delta cursor stays in the log file")
}

// Validates: R-6.6.30
func TestConsoleLogHandler_JournalFilesNoticeAtNoticePriority(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(newConsoleLogHandler(&buf, loglevel.Notice, consoleLogJournal))
	logger.Log(t.Context(), loglevel.Notice, "Checking OneDrive for changes works again")
	logger.Info("hidden at the default level")

	assert.Equal(t, "<5>Checking OneDrive for changes works again\n", buf.String())
}

// Validates: R-6.6.30
func TestWithConsoleTimestamps_StampsHumanLinesOnly(t *testing.T) {
	t.Parallel()

	var console, file bytes.Buffer
	now := func() time.Time { return time.Date(2026, 10, 29, 6, 0, 21, 0, time.UTC) }
	logger := slog.New(&multiHandler{handlers: []slog.Handler{
		newConsoleLogHandler(&console, loglevel.Notice, consoleLogHuman),
		slog.NewJSONHandler(&file, &slog.HandlerOptions{ReplaceAttr: loglevel.ReplaceLevelName}),
	}})

	stamped := withConsoleTimestamps(logger, now).With("drive", "personal")
	stamped.Warn("Can't check OneDrive for changes; retrying")
	stamped.Log(t.Context(), loglevel.Notice, "Checking OneDrive for changes works again")

	assert.Equal(t,
		"[06:00:21] Warning: Can't check OneDrive for changes; retrying (drive=personal)\n"+
			"[06:00:21] Checking OneDrive for changes works again (drive=personal)\n",
		console.String())
	assert.NotContains(t, file.String(), "[06:00:21]", "the log file keeps its own timestamps")
	assert.Contains(t, file.String(), `"level":"NOTICE"`)

	journal := newConsoleLogHandler(&bytes.Buffer{}, loglevel.Notice, consoleLogJournal)
	assert.Same(t, journal, journal.withTimestamps(now), "journald adds its own timestamps")
}
