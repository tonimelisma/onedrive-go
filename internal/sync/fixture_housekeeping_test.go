package sync

import (
	"bytes"
	"log/slog"
)

func newDebugLogBuffer() (*bytes.Buffer, *slog.Logger) {
	var buf bytes.Buffer

	return &buf, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
