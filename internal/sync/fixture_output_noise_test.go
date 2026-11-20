package sync

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	stdsync "sync"
)

// syncLockedBuffer is a bytes.Buffer safe for slog handlers written from
// several goroutines.
type syncLockedBuffer struct {
	mu  stdsync.Mutex // guards buf
	buf bytes.Buffer
}

func (b *syncLockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n, err := b.buf.Write(p)
	if err != nil {
		return n, fmt.Errorf("write log buffer: %w", err)
	}

	return n, nil
}

func (b *syncLockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func newCapturedLogger() (*slog.Logger, *syncLockedBuffer) {
	buf := &syncLockedBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

func countLevel(output, level string) int {
	return strings.Count(output, "level="+level+" ")
}
