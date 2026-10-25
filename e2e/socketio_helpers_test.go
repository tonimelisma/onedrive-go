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

// ensureDebugEvents makes sure a daemon started with env writes the
// structured debug-event stream, returning the env to start it with and the
// stream's path. An env that already names a stream keeps it, so a test that
// set one up for its own assertions and a daemon helper share one file.
func ensureDebugEvents(t *testing.T, env map[string]string) (map[string]string, string) {
	t.Helper()

	if path := env["ONEDRIVE_TEST_DEBUG_EVENTS_PATH"]; path != "" {
		return env, path
	}

	return withSocketIODebugEvents(t, env)
}

// waitForDaemonReady waits until a watch daemon has started its first
// observer. Readiness comes from the structured debug-event stream (see
// ensureDebugEvents), not from log wording: rewording a log line must not be
// able to break, or falsely satisfy, a test's synchronization.
func waitForDaemonReady(t *testing.T, eventsPath string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, event := range readSocketIODebugEvents(t, eventsPath) {
			if string(event.Type) == "observer_started" {
				t.Logf("daemon ready (%s observer started)", event.Observer)
				return
			}
		}

		sleepForLiveTestPropagation(500 * time.Millisecond)
	}

	require.FailNowf(t, "daemon did not become ready",
		"no observer started within %v; events: %+v", timeout, readSocketIODebugEvents(t, eventsPath))
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
				if isExpectedMountRootWebsocketFallback(event) {
					continue
				}
				require.FailNowf(t, "socket.io fell back to polling", "%+v", event)
			}
		}

		sleepForLiveTestPropagation(500 * time.Millisecond)
	}

	require.FailNowf(t, "socket.io did not connect within the startup window", "events: %+v", readSocketIODebugEvents(t, eventsPath))
	return syncengine.DebugEvent{}
}

func isExpectedMountRootWebsocketFallback(event syncengine.DebugEvent) bool {
	return event.Note == "mount_root"
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
