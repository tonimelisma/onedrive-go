package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// A run that died after setting a file aside leaves it under a hidden name in
// the sync folder. It is user content: every full scan reports it, nothing
// uploads it, and housekeeping never deletes it.
//
// Validates: R-6.4.9
func TestFullScan_LeftoverSetAsideFileIsReportedNotDeleted(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	eng, syncRoot := newTestEngine(t, remote.client(t))
	leftover := filepath.Join(syncRoot, ".onedrive-go.0123456789abcdef.detach")
	require.NoError(t, os.WriteFile(leftover, []byte("half-removed\n"), 0o600))

	runScenarioPass(t, eng)

	assert.FileExists(t, leftover, "an interrupted removal is never cleaned up automatically")
	assert.NotContains(t, remote.tree(), ".onedrive-go.0123456789abcdef.detach", "it is never uploaded")
	issues, err := eng.baseline.ListObservationIssues(t.Context())
	require.NoError(t, err)
	found := false
	for i := range issues {
		if issues[i].Path == ".onedrive-go.0123456789abcdef.detach" && issues[i].IssueType == syncdomain.IssueLocalInterruptedRemoval {
			found = true
		}
	}
	assert.True(t, found, "the leftover is reported as a durable issue naming its path: %+v", issues)
}
