package sync

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	stdsync "sync"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

const debugEventFilePerm = 0o600

type debugEventFileSink struct {
	mu     stdsync.Mutex
	file   *os.File
	logger *slog.Logger
}

func newDebugEventFileSink(path string, logger *slog.Logger) (*debugEventFileSink, error) {
	if path == "" {
		return nil, fmt.Errorf("debug event path is empty")
	}
	if logger == nil {
		logger = slog.Default()
	}

	file, err := localpath.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, debugEventFilePerm)
	if err != nil {
		return nil, fmt.Errorf("open debug event file: %w", err)
	}

	return &debugEventFileSink{
		file:   file,
		logger: logger,
	}, nil
}

//nolint:gocritic // Value semantics are intentional here so hooks observe immutable snapshots.
func (s *debugEventFileSink) Hook(event DebugEvent) {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := json.NewEncoder(s.file).Encode(event); err != nil {
		s.logger.Warn("debug event sink write failed",
			slog.String("error", err.Error()),
		)
	}
}

func (s *debugEventFileSink) Close() error {
	if s == nil || s.file == nil {
		return nil
	}

	if err := s.file.Close(); err != nil {
		return fmt.Errorf("close debug event file: %w", err)
	}

	return nil
}

// NewDebugEventFileHook opens an NDJSON debug-event sink and returns a hook
// suitable for Engine.SetDebugEventHook plus its closer.
func NewDebugEventFileHook(path string, logger *slog.Logger) (func(DebugEvent), func() error, error) {
	sink, err := newDebugEventFileSink(path, logger)
	if err != nil {
		return nil, nil, err
	}

	return sink.Hook, sink.Close, nil
}
