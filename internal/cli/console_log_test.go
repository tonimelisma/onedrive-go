package cli

import (
	"bytes"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/graph"
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
