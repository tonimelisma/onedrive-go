package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func TestPrintRunOnceResult_MatchesReportsBySelectionIndex(t *testing.T) {
	t.Parallel()

	cc, status := statusCC()
	cid := driveid.MustCanonicalID("personal:render@example.com")

	printRunOnceResult(multisync.RunOnceResult{
		Startup: multisync.StartupSelectionSummary{
			Results: []multisync.MountStartupResult{
				{
					SelectionIndex: 0,
					Identity:       testStandaloneMountIdentity(cid),
					DisplayName:    "First selector",
					Status:         multisync.MountStartupRunnable,
				},
				{
					SelectionIndex: 1,
					Identity:       testStandaloneMountIdentity(cid),
					DisplayName:    "Second selector",
					Status:         multisync.MountStartupRunnable,
				},
			},
		},
		Reports: []*multisync.MountReport{
			{
				SelectionIndex: 1,
				Identity:       testStandaloneMountIdentity(cid),
				DisplayName:    "Second selector",
				Report: &syncengine.Report{
					Mode: syncengine.SyncUploadOnly,
				},
			},
			{
				SelectionIndex: 0,
				Identity:       testStandaloneMountIdentity(cid),
				DisplayName:    "First selector",
				Report: &syncengine.Report{
					Mode: syncengine.SyncDownloadOnly,
				},
			},
		},
	}, cc)

	output := status.String()
	firstHeader := strings.Index(output, "--- First selector ---")
	firstMode := strings.Index(output, "Mode: download-only")
	secondHeader := strings.Index(output, "--- Second selector ---")
	secondMode := strings.Index(output, "Mode: upload-only")

	require.NotEqual(t, -1, firstHeader)
	require.NotEqual(t, -1, firstMode)
	require.NotEqual(t, -1, secondHeader)
	require.NotEqual(t, -1, secondMode)
	assert.Less(t, firstHeader, firstMode)
	assert.Less(t, firstMode, secondHeader)
	assert.Less(t, secondHeader, secondMode)
}

func TestPrintRunOnceResult_RendersDuplicateSelectionReportsWithoutDroppingParentReport(t *testing.T) {
	t.Parallel()

	cc, status := statusCC()
	cid := driveid.MustCanonicalID("personal:render@example.com")
	identity := testStandaloneMountIdentity(cid)

	printRunOnceResult(multisync.RunOnceResult{
		Startup: multisync.StartupSelectionSummary{
			Results: []multisync.MountStartupResult{{
				SelectionIndex: 0,
				Identity:       identity,
				DisplayName:    "Parent selector",
				Status:         multisync.MountStartupRunnable,
			}},
		},
		Reports: []*multisync.MountReport{
			{
				SelectionIndex: 0,
				Identity:       identity,
				DisplayName:    "Parent selector",
				Report: &syncengine.Report{
					Mode: syncengine.SyncDownloadOnly,
				},
			},
			{
				SelectionIndex: 0,
				Identity:       identity,
				DisplayName:    "Parent selector",
				Err:            errors.New("shortcut child finalization before parent safe point"),
			},
		},
	}, cc)

	output := status.String()
	modeIndex := strings.Index(output, "Mode: download-only")
	errorIndex := strings.Index(output, "shortcut child finalization before parent safe point")

	require.NotEqual(t, -1, modeIndex)
	require.NotEqual(t, -1, errorIndex)
	assert.Less(t, modeIndex, errorIndex)
}
