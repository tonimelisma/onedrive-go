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

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
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

// Debug-event types the e2e helpers react to; they mirror the unexported
// engineDebugEvent* values in internal/sync.
const (
	debugEventObserverStarted           = "observer_started"
	debugEventWebsocketConnected        = "websocket_connected"
	debugEventWebsocketEndpointFailed   = "websocket_endpoint_fetch_failed"
	debugEventWebsocketConnectFailed    = "websocket_connect_failed"
	debugEventWebsocketFallback         = "websocket_fallback"
	debugEventWebsocketNotificationWake = "websocket_notification_wake"
)

// daemonReadyTimeout bounds how long a watch daemon may take to start its
// first observer.
const daemonReadyTimeout = 30 * time.Second

// waitForDaemonReady waits until a watch daemon has started its first
// observer. Readiness comes from the structured debug-event stream (see
// ensureDebugEvents), not from log wording: rewording a log line must not be
// able to break, or falsely satisfy, a test's synchronization.
func waitForDaemonReady(t *testing.T, eventsPath string) {
	t.Helper()

	deadline := time.Now().Add(daemonReadyTimeout)
	for time.Now().Before(deadline) {
		events := readSocketIODebugEvents(t, eventsPath)
		for i := range events {
			if string(events[i].Type) == debugEventObserverStarted {
				t.Logf("daemon ready (%s observer started)", events[i].Observer)
				return
			}
		}

		sleepForLiveTestPropagation(500 * time.Millisecond)
	}

	require.FailNowf(t, "daemon did not become ready",
		"no observer started within %v; %s", daemonReadyTimeout,
		debugEventsSummary(suiteDriveRoles, readSocketIODebugEvents(t, eventsPath)))
}

func readSocketIODebugEvents(t *testing.T, path string) []syncdomain.DebugEvent {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // the debug-event stream in the test's own temp dir
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		requireNoLocalError(t, err, "read the debug-event stream")
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}

	events := make([]syncdomain.DebugEvent, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var event syncdomain.DebugEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		events = append(events, event)
	}

	return events
}

func waitForSocketIOConnected(t *testing.T, eventsPath string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		events := readSocketIODebugEvents(t, eventsPath)
		for i := range events {
			event := &events[i]
			switch string(event.Type) {
			case debugEventWebsocketConnected:
				return
			case debugEventWebsocketEndpointFailed:
				require.FailNowf(t, "socket.io endpoint fetch failed", "%s", debugEventSummary(suiteDriveRoles, event))
			case debugEventWebsocketConnectFailed:
				require.FailNowf(t, "socket.io connection failed", "%s", debugEventSummary(suiteDriveRoles, event))
			case debugEventWebsocketFallback:
				if isExpectedMountRootWebsocketFallback(event) {
					continue
				}
				require.FailNowf(t, "socket.io fell back to polling", "%s", debugEventSummary(suiteDriveRoles, event))
			}
		}

		sleepForLiveTestPropagation(500 * time.Millisecond)
	}

	require.FailNowf(t, "socket.io did not connect within the startup window", "%s",
		debugEventsSummary(suiteDriveRoles, readSocketIODebugEvents(t, eventsPath)))
}

func isExpectedMountRootWebsocketFallback(event *syncdomain.DebugEvent) bool {
	return event.Note == "mount_root"
}

func waitForObserverStarted(
	t *testing.T,
	eventsPath string,
	observer string,
	timeout time.Duration,
) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		events := readSocketIODebugEvents(t, eventsPath)
		for i := range events {
			if string(events[i].Type) == debugEventObserverStarted && events[i].Observer == observer {
				return
			}
		}

		sleepForLiveTestPropagation(500 * time.Millisecond)
	}

	require.FailNowf(
		t,
		"observer did not start within the startup window",
		"observer=%s %s",
		observer,
		debugEventsSummary(suiteDriveRoles, readSocketIODebugEvents(t, eventsPath)),
	)
}

func waitForSocketIOWakeAndLocalFileAfter(
	t *testing.T,
	eventsPath string,
	afterCount int,
	localPath string,
	expected string,
	timeout time.Duration,
) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for attempt := 0; ; attempt++ {
		events := readSocketIODebugEvents(t, eventsPath)
		wakeSeen := false
		for idx := afterCount; idx < len(events); idx++ {
			event := &events[idx]
			switch string(event.Type) {
			case debugEventWebsocketNotificationWake:
				wakeSeen = true
			case debugEventWebsocketEndpointFailed:
				require.FailNowf(t, "socket.io endpoint fetch failed", "%s", debugEventSummary(suiteDriveRoles, event))
			case debugEventWebsocketConnectFailed:
				require.FailNowf(t, "socket.io connection failed", "%s", debugEventSummary(suiteDriveRoles, event))
			}
		}

		data, err := os.ReadFile(localPath) //nolint:gosec // a file inside the test's own sync dir
		if wakeSeen && err == nil && string(data) == expected {
			return
		}

		if time.Now().After(deadline) {
			fileState := fmt.Sprintf("missing: %v", err)
			if err == nil {
				fileState = fmt.Sprintf("content=%q", string(data))
			}

			require.FailNowf(
				t,
				"socket.io wake and local convergence did not arrive",
				"path=%s expected=%q file=%s %s",
				localPath,
				expected,
				fileState,
				debugEventsSummary(suiteDriveRoles, events),
			)
		}

		sleepForLiveTestPropagation(pollBackoff(attempt))
	}
}

func assertNoSocketIOConnected(t *testing.T, eventsPath string) {
	t.Helper()

	events := readSocketIODebugEvents(t, eventsPath)
	assert.False(t, containsSocketIOEvent(events, debugEventWebsocketConnected))
}

func containsSocketIOEvent(events []syncdomain.DebugEvent, target string) bool {
	for i := range events {
		if string(events[i].Type) == target {
			return true
		}
	}

	return false
}
