package cli

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// leftWorkTestActivities are file events of an earlier pass: a failure, a
// failure kept for retry, and a success. They print as they happen, and
// change none of the numbers of what the run left.
func leftWorkTestActivities() []syncengine.Activity {
	return []syncengine.Activity{
		{Kind: syncengine.ActivityUpload, Path: "a.txt", Err: os.ErrPermission},
		{Kind: syncengine.ActivityUpload, Path: "b.txt", WillRetry: true, Err: os.ErrDeadlineExceeded},
		{Kind: syncengine.ActivityUpload, Path: "c.txt", Success: true},
	}
}

// Validates: R-6.6.28, R-6.6.33
//
// What a drive left is the report's alone: the text summary, the JSON
// summary and the exit status give the numbers the report's last pass
// counts, and the file events the console printed during the run do not
// change them.
func TestSyncConsole_LeftWorkComesFromTheReportAlone(t *testing.T) {
	t.Parallel()

	report := &syncengine.Report{
		Duration:        2 * time.Second,
		Uploads:         3,
		Succeeded:       1,
		LeftForNextSync: 1,
		UnsyncedIssues:  1,
	}

	c, out, _ := testSyncConsole()
	c.ActivityHook()
	for _, a := range leftWorkTestActivities() {
		c.Activity(mountActivity(a))
	}
	out.Reset()
	result := singleMountResult(report)
	left := c.Summary(result)
	assert.Equal(t, "Done in 2.0 s: uploaded 1 file; 1 will be retried on the next sync; "+
		"1 still not synced (see 'onedrive-go status').\n", out.String())
	err := runOnceResultError(result, left)
	require.ErrorIs(t, err, errSyncLeftWork)
	assert.Equal(t, "sync left 2 files unsynced; run 'onedrive-go status' for details", err.Error())

	c, out, _ = testSyncConsole()
	c.json = true
	c.ActivityHook()
	for _, a := range leftWorkTestActivities() {
		c.Activity(mountActivity(a))
	}
	out.Reset()
	c.Summary(result)
	var summary syncJSONEvent
	require.NoError(t, json.Unmarshal(out.Bytes(), &summary))
	assert.Equal(t, "incomplete", summary.Status)
	require.NotNil(t, summary.Failed)
	require.NotNil(t, summary.WillRetry)
	require.NotNil(t, summary.StillOpen)
	assert.Zero(t, *summary.Failed)
	assert.Equal(t, 1, *summary.WillRetry)
	assert.Equal(t, 1, *summary.StillOpen)
}
