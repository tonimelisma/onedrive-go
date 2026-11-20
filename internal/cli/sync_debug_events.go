package cli

import (
	"fmt"
	"log/slog"
	"os"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

const testDebugEventsPathEnv = "ONEDRIVE_TEST_DEBUG_EVENTS_PATH"

func openSyncDebugEventHookFromEnv(logger *slog.Logger) (func(syncdomain.DebugEvent), func() error, error) {
	path, ok := os.LookupEnv(testDebugEventsPathEnv)
	if !ok || path == "" {
		return nil, nil, nil
	}

	hook, closeFn, err := syncengine.NewDebugEventFileHook(path, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("open debug event file hook: %w", err)
	}

	return hook, closeFn, nil
}
