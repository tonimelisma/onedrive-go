package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// An applied move, in either direction and of either kind, publishes the
// destination's baseline row once: the pass that applies it ends with nothing
// left to publish. A remote file move applied locally used to keep the source
// baseline's inode change time, which rename(2) updates on Linux, so every
// such move was followed by a second, metadata-only baseline update.
//
// Validates: R-2.17.3
func TestRunOnce_AppliedMoveLeavesTheBaselineCurrent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		move func(t *testing.T, remote *moveScenarioRemote, syncRoot string)
		want string
	}{
		{
			name: "remote file move applied locally",
			move: func(_ *testing.T, remote *moveScenarioRemote, _ string) {
				remote.move("item-f", "item-b", "g.txt")
			},
			want: filepath.Join("B", "g.txt"),
		},
		{
			name: "remote folder move applied locally",
			move: func(_ *testing.T, remote *moveScenarioRemote, _ string) {
				remote.move("item-a", "root", "Z")
			},
			want: filepath.Join("Z", "f.txt"),
		},
		{
			name: "local file move applied online",
			move: func(t *testing.T, _ *moveScenarioRemote, syncRoot string) {
				t.Helper()
				require.NoError(t, os.Rename(filepath.Join(syncRoot, "A", "f.txt"), filepath.Join(syncRoot, "B", "g.txt")))
			},
			want: filepath.Join("B", "g.txt"),
		},
		{
			name: "local folder move applied online",
			move: func(t *testing.T, _ *moveScenarioRemote, syncRoot string) {
				t.Helper()
				require.NoError(t, os.Rename(filepath.Join(syncRoot, "A"), filepath.Join(syncRoot, "Z")))
			},
			want: filepath.Join("Z", "f.txt"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
			remote.addFolder("item-a", "root", "A")
			remote.addFolder("item-b", "root", "B")
			remote.addFile(t, "item-f", "item-a", "f.txt", "content\n")
			eng, syncRoot := newTestEngine(t, remote.client(t))
			runScenarioPass(t, eng)

			tt.move(t, remote, syncRoot)
			report := runScenarioPass(t, eng)
			require.FileExists(t, filepath.Join(syncRoot, tt.want))
			assert.Equal(t, 1, report.Moves)
			assert.Zero(t, report.BaselineUpdates, "the move publishes the destination row itself: %+v", report)

			next, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{DryRun: true})
			require.NoError(t, err)
			assert.Zero(t, next.BaselineUpdates+next.Uploads+next.Downloads+next.Moves+next.FolderCreates+
				next.LocalDeletes+next.RemoteDeletes, "nothing is left to publish: %+v", next)
		})
	}
}
