package multisync

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// A removed shared folder whose final sync does not finish keeps its local
// copy. The user is told so once at Info, again only when the reason
// changes, and every unchanged repeat (watch re-runs the drain each pass)
// stays at Debug.
//
// Validates: R-2.4.8, R-6.6.30
func TestReportKeptFinalDrains_NoticesOnceUntilReasonChanges(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	parent := testStandaloneMount(t, "personal:kept-drain@example.com", "Parent")
	child := testChildRecord(t, mountID(parent.CanonicalID.String()), "binding-kept", "Shortcuts/Docs")
	child.Mode = syncengine.ShortcutChildRunModeFinalDrain
	decisions, err := buildRuntimeWork([]StandaloneMountConfig{parent}, testParentSnapshots(&parent, child), t.TempDir())
	require.NoError(t, err)
	var childMount *mountSpec
	for i := range decisions.Mounts {
		if decisions.Mounts[i].isFinalDrainChild() {
			childMount = decisions.Mounts[i]
		}
	}
	require.NotNil(t, childMount)

	capture, logger := synctest.NewLogCapture()
	cfg := testOrchestratorConfig(t, parent)
	cfg.Logger = logger
	orch := NewOrchestrator(cfg)

	blocked := []shortcutChildDrainResult{{
		MountID: childMount.id().String(),
		Status:  shortcutChildDrainBlocked,
		Detail:  "child sync: 2 failed",
	}}
	for range 3 {
		orch.reportKeptFinalDrains(blocked, decisions.Mounts)
	}

	notices := keptDrainNotices(capture)
	require.Len(t, notices, 1, "an unchanged kept copy is announced once")
	assert.Contains(t, notices[0].Message, "Keeping the local copy of removed shared folder "+childMount.label())
	assert.Contains(t, notices[0].Message, ": 2 failed;", "the notice says what keeps the copy")
	assert.NotContains(t, notices[0].Message, childMount.id().String(), "user text names the folder, not the mount ID")
	assert.Equal(t, slog.LevelInfo, notices[0].Level)

	unavailable := []shortcutChildDrainResult{{
		MountID: childMount.id().String(),
		Status:  shortcutChildDrainRootUnavailable,
		Detail:  "sync folder unavailable",
	}}
	orch.reportKeptFinalDrains(unavailable, decisions.Mounts)
	notices = keptDrainNotices(capture)
	require.Len(t, notices, 2, "a changed reason is announced again")
	assert.Contains(t, notices[1].Message, "its local folder is unavailable")

	clean := []shortcutChildDrainResult{{MountID: childMount.id().String(), Status: shortcutChildDrainClean}}
	orch.reportKeptFinalDrains(clean, decisions.Mounts)
	assert.Len(t, keptDrainNotices(capture), 2, "a clean drain is not a kept copy")
	assert.Empty(t, capture.AtLeast(slog.LevelWarn))
}

func keptDrainNotices(capture *synctest.LogCapture) []synctest.CapturedRecord {
	var out []synctest.CapturedRecord
	for _, record := range capture.AtLeast(slog.LevelInfo) {
		if strings.Contains(record.Message, "removed shared folder") {
			out = append(out, record)
		}
	}
	return out
}
