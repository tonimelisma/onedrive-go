package syncfixture

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	stdsync "sync"
)

// SyncLockedBuffer is a bytes.Buffer safe for slog handlers written from
// several goroutines.
type SyncLockedBuffer struct {
	mu  stdsync.Mutex // guards buf
	buf bytes.Buffer
}

func (b *SyncLockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n, err := b.buf.Write(p)
	if err != nil {
		return n, fmt.Errorf("write log buffer: %w", err)
	}

	return n, nil
}

func (b *SyncLockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func NewCapturedLogger() (*slog.Logger, *SyncLockedBuffer) {
	buf := &SyncLockedBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

func CountLevel(output, level string) int {
	return strings.Count(output, "level="+level+" ")
}
