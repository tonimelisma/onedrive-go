package syncfixture

import (
	"bytes"
	"log/slog"
)

func NewDebugLogBuffer() (*bytes.Buffer, *slog.Logger) {
	var buf bytes.Buffer

	return &buf, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
