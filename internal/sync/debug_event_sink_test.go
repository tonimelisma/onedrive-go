package sync

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewDebugEventFileHook_WritesNDJSON(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "events.ndjson")
	hook, closeFn, err := NewDebugEventFileHook(path, testLogger(t))
	require.NoError(t, err)

	hook(DebugEvent{
		Type:    engineDebugEventWebsocketConnected,
		DriveID: "drive-123",
		Note:    "sid=sid-1",
	})
	require.NoError(t, closeFn())

	//nolint:gosec // Test reads the temp file it just wrote.
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var event DebugEvent
	require.NoError(t, json.Unmarshal(data, &event))
	assert.Equal(t, engineDebugEventWebsocketConnected, event.Type)
	assert.Equal(t, "drive-123", event.DriveID)
	assert.Equal(t, "sid=sid-1", event.Note)
}
