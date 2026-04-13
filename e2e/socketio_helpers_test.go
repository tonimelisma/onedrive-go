//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func withSocketIODebugEvents(t *testing.T, env map[string]string) (map[string]string, string) {
	t.Helper()

	cloned := make(map[string]string, len(env)+1)
	for key, value := range env {
		cloned[key] = value
	}

	path := filepath.Join(t.TempDir(), "socketio-events.ndjson")
	cloned["ONEDRIVE_TEST_DEBUG_EVENTS_PATH"] = path

	return cloned, path
}

func readSocketIODebugEvents(t *testing.T, path string) []syncengine.DebugEvent {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		require.NoError(t, err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}

	events := make([]syncengine.DebugEvent, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var event syncengine.DebugEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		events = append(events, event)
	}

	return events
}

func waitForSocketIOConnected(t *testing.T, eventsPath string, timeout time.Duration) syncengine.DebugEvent {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		events := readSocketIODebugEvents(t, eventsPath)
		for _, event := range events {
			switch string(event.Type) {
			case "websocket_connected":
				return event
			case "websocket_endpoint_fetch_failed":
				require.FailNowf(t, "socket.io endpoint fetch failed", "%+v", event)
			case "websocket_connect_failed":
				require.FailNowf(t, "socket.io connection failed", "%+v", event)
			case "websocket_fallback":
				require.FailNowf(t, "socket.io fell back to polling", "%+v", event)
			}
		}

		sleepForLiveTestPropagation(500 * time.Millisecond)
	}

	require.FailNowf(t, "socket.io did not connect within the startup window", "events: %+v", readSocketIODebugEvents(t, eventsPath))
	return syncengine.DebugEvent{}
}

func waitForObserverStarted(
	t *testing.T,
	eventsPath string,
	observer string,
	timeout time.Duration,
) syncengine.DebugEvent {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		events := readSocketIODebugEvents(t, eventsPath)
		for _, event := range events {
			if string(event.Type) == "observer_started" && event.Observer == observer {
				return event
			}
		}

		sleepForLiveTestPropagation(500 * time.Millisecond)
	}

	require.FailNowf(
		t,
		"observer did not start within the startup window",
		"observer=%s events=%+v",
		observer,
		readSocketIODebugEvents(t, eventsPath),
	)
	return syncengine.DebugEvent{}
}

func waitForSocketIOEventAfter(
	t *testing.T,
	eventsPath string,
	afterCount int,
	timeout time.Duration,
	target string,
) syncengine.DebugEvent {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		events := readSocketIODebugEvents(t, eventsPath)
		for idx := afterCount; idx < len(events); idx++ {
			event := events[idx]
			switch string(event.Type) {
			case target:
				return event
			case "websocket_endpoint_fetch_failed":
				require.FailNowf(t, "socket.io endpoint fetch failed", "%+v", event)
			case "websocket_connect_failed":
				require.FailNowf(t, "socket.io connection failed", "%+v", event)
			}
		}

		sleepForLiveTestPropagation(500 * time.Millisecond)
	}

	require.FailNowf(t, "socket.io event did not arrive", "target=%s events=%+v", target, readSocketIODebugEvents(t, eventsPath))
	return syncengine.DebugEvent{}
}

func waitForSocketIOWakeAndLocalFileAfter(
	t *testing.T,
	eventsPath string,
	afterCount int,
	localPath string,
	expected string,
	timeout time.Duration,
) syncengine.DebugEvent {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for attempt := 0; ; attempt++ {
		events := readSocketIODebugEvents(t, eventsPath)
		var wake syncengine.DebugEvent
		wakeSeen := false
		for idx := afterCount; idx < len(events); idx++ {
			event := events[idx]
			switch string(event.Type) {
			case "websocket_notification_wake":
				wake = event
				wakeSeen = true
			case "websocket_endpoint_fetch_failed":
				require.FailNowf(t, "socket.io endpoint fetch failed", "%+v", event)
			case "websocket_connect_failed":
				require.FailNowf(t, "socket.io connection failed", "%+v", event)
			}
		}

		data, err := os.ReadFile(localPath)
		if wakeSeen && err == nil && string(data) == expected {
			return wake
		}

		if time.Now().After(deadline) {
			fileState := fmt.Sprintf("missing: %v", err)
			if err == nil {
				fileState = fmt.Sprintf("content=%q", string(data))
			}

			require.FailNowf(
				t,
				"socket.io wake and local convergence did not arrive",
				"path=%s expected=%q file=%s events=%+v",
				localPath,
				expected,
				fileState,
				events,
			)
		}

		sleepForLiveTestPropagation(pollBackoff(attempt))
	}
}

func assertNoSocketIOConnected(t *testing.T, eventsPath string) {
	t.Helper()

	events := readSocketIODebugEvents(t, eventsPath)
	assert.False(t, containsSocketIOEvent(events, "websocket_connected"))
}

func containsSocketIOEvent(events []syncengine.DebugEvent, target string) bool {
	for _, event := range events {
		if string(event.Type) == target {
			return true
		}
	}

	return false
}
