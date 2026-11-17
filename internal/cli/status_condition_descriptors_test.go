package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

const (
	issueTypeRemoteReadDenied   = "remote_read_denied"
	issueTypeRemoteRootNotFound = "remote_root_not_found"
)

// rootReadIssueSnapshot is a store holding one remote read issue of the given
// type at path, under the read scope of boundary ("" for the root).
func rootReadIssueSnapshot(issueType, path, boundary string) *syncengine.DriveStatusSnapshot {
	return &syncengine.DriveStatusSnapshot{
		ObservationIssues: []syncengine.ObservationIssueRow{{
			Path:      path,
			IssueType: syncengine.IssueType(issueType),
			ScopeKey:  syncengine.ScopeKey{Kind: syncengine.ScopePermRemoteRead, Param: boundary},
		}},
	}
}

// Validates: R-2.10.4
//
// What status says about a root OneDrive refused to read depends on the
// answer and on what the root is. At a shared folder's root, "not found"
// says the shared folder is gone and "forbidden" says it cannot be read, each
// with its one next step; at a drive's own root "not found" says the folder
// the drive syncs was not found, with no guess at why. "Forbidden" anywhere
// but a shared folder's root keeps its text. The text output and --json say
// the same, and --json names the answer in the issue's type.
func TestBuildSyncStateInfo_RootReadFailureSaysWhatTheRootIs(t *testing.T) {
	t.Parallel()

	for _, tc := range rootReadFailureCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			info := buildSyncStateInfo(tc.snapshot, tc.root, false, defaultVisiblePaths)
			require.Len(t, info.Conditions, 1)

			var text bytes.Buffer
			require.NoError(t, printSyncStateText(&text, "", &info))
			assert.Contains(t, text.String(), tc.want.Title+": 1 item\n")
			assert.Contains(t, text.String(), "  "+tc.want.Reason+"\n")
			assert.Contains(t, text.String(), "  Action: "+tc.want.Action+"\n")

			encoded, err := json.Marshal(info.Conditions[0])
			require.NoError(t, err)
			var decoded map[string]any
			require.NoError(t, json.Unmarshal(encoded, &decoded))
			assert.Equal(t, tc.wantType, decoded["type"])
			assert.Equal(t, tc.want.Title, decoded["title"])
			assert.Equal(t, tc.want.Reason, decoded["reason"])
			assert.Equal(t, tc.want.Action, decoded["action"])
		})
	}
}

// Validates: R-6.2.15
//
// A folder sync crossed into as the mount point of another filesystem, found
// not mounted now, is its own condition: status says what happened, that the
// synced content is kept and nothing is deleted on OneDrive, what clears it
// without losing anything (TestRunOnce_ADiskMountedAgainLosesNothing), and
// what `drive rebind-root` leads to: it forgets every disk listed that is not
// mounted, and sync then deletes each such folder's content on OneDrive too,
// holding the deletes for confirmation only when they are many
// (TestRunOnce_ARebindAfterADiskIsGoneDeletesItsContentOnOneDrive). It does
// not suggest cross_filesystems, the setting that made sync cross it.
func TestBuildSyncStateInfo_AMountPointNotMountedSaysWhatClearsIt(t *testing.T) {
	t.Parallel()

	snapshot := &syncengine.DriveStatusSnapshot{
		ObservationIssues: []syncengine.ObservationIssueRow{{
			Path:      "nas",
			IssueType: syncengine.IssueType("local_mount_missing"),
			ScopeKey:  syncengine.ScopeKey{Kind: syncengine.ScopePermDirRead, Param: "nas"},
		}},
	}
	info := buildSyncStateInfo(snapshot, statusStoreRootDrive, false, defaultVisiblePaths)
	require.Len(t, info.Conditions, 1)

	condition := info.Conditions[0]
	assert.Equal(t, "local_mount_missing", condition.ConditionType)
	assert.Equal(t, "Disk not mounted", condition.Title)
	assert.Equal(t, "This folder was the mount point of another disk or filesystem, and that disk is not mounted now. "+
		"Its synced content is kept, and nothing is deleted on OneDrive.", condition.Reason)
	assert.Equal(t, "Mount the disk again; that clears it and loses nothing. If the disk is gone for good, copy what "+
		"you need from this folder on OneDrive first: 'onedrive-go drive rebind-root' forgets every disk listed "+
		"here that is not mounted, and sync then deletes each such folder's content on OneDrive too, holding the "+
		"deletes for confirmation only when they are many. Mount every disk you still use before you rebind.",
		condition.Action)
	assert.NotContains(t, condition.Reason+condition.Action, "cross_filesystems")
	assert.Equal(t, []string{"nas"}, condition.Paths)
}

type rootReadFailureCase struct {
	name     string
	root     statusStoreRoot
	snapshot *syncengine.DriveStatusSnapshot
	wantType string
	want     statusConditionDescriptor
}

func rootReadFailureCases() []rootReadFailureCase {
	// Where the shared-folder text does not apply, the condition keeps its
	// own text, which TestDescribeStatusCondition_CoversFamiliesAndFallback pins.
	readBlocked := describeStatusCondition(syncengine.ConditionRemoteReadDenied)

	return []rootReadFailureCase{
		{
			name:     "shared folder root not found",
			root:     statusStoreRootSharedFolder,
			snapshot: rootReadIssueSnapshot(issueTypeRemoteRootNotFound, "/", ""),
			wantType: issueTypeRemoteRootNotFound,
			want: newStatusConditionDescriptor(
				"Shared folder gone",
				"This shared folder was deleted by its owner, or it is no longer shared with this account.",
				"Remove its shortcut from your OneDrive; that ends this report.",
			),
		},
		{
			name:     "shared folder root forbidden",
			root:     statusStoreRootSharedFolder,
			snapshot: rootReadIssueSnapshot(issueTypeRemoteReadDenied, "/", ""),
			wantType: issueTypeRemoteReadDenied,
			want: newStatusConditionDescriptor(
				"Shared folder cannot be read",
				"This account no longer has permission to read this shared folder.",
				"Ask the owner for access, or remove its shortcut from your OneDrive.",
			),
		},
		{
			name:     "drive root not found",
			root:     statusStoreRootDrive,
			snapshot: rootReadIssueSnapshot(issueTypeRemoteRootNotFound, "/", ""),
			wantType: issueTypeRemoteRootNotFound,
			want: newStatusConditionDescriptor(
				"Synced folder not found",
				"OneDrive answered that the folder this drive syncs was not found.",
				"Check on the OneDrive website that the folder still exists. If it is gone, "+
					"remove the drive with 'onedrive-go drive remove --drive <drive>'.",
			),
		},
		{
			name:     "drive root forbidden",
			root:     statusStoreRootDrive,
			snapshot: rootReadIssueSnapshot(issueTypeRemoteReadDenied, "/", ""),
			wantType: issueTypeRemoteReadDenied,
			want:     readBlocked,
		},
		{
			name:     "forbidden below a shared folder's root",
			root:     statusStoreRootSharedFolder,
			snapshot: rootReadIssueSnapshot(issueTypeRemoteReadDenied, "Docs", "Docs"),
			wantType: issueTypeRemoteReadDenied,
			want:     readBlocked,
		},
	}
}

// rootRecordingQuerier answers every sync state query with an empty state and
// records the root each query named.
type rootRecordingQuerier struct {
	roots []statusStoreRoot
}

func (q *rootRecordingQuerier) QuerySyncState(
	_ context.Context, _ string, root statusStoreRoot, _ syncengine.ContentFilterConfig,
) *syncStateInfo {
	q.roots = append(q.roots, root)
	return &syncStateInfo{}
}

// Validates: R-2.10.4
//
// Status reads a shortcut child's store as a shared folder's, so a read
// failure at its root is told as the shared folder's.
func TestBuildChildStatusDrive_ReadsTheStoreAsASharedFolder(t *testing.T) {
	t.Parallel()

	parentID := driveid.MustCanonicalID("personal:owner@example.com")
	querier := &rootRecordingQuerier{}
	child := &childMountStatusInput{
		ParentID: parentID,
		Root: syncengine.ShortcutRootStatusView{
			MountID:     config.ChildMountID(parentID.String(), "binding-docs"),
			DisplayName: "Docs",
		},
	}

	status := buildChildStatusDrive(t.Context(), &config.Drive{}, child, querier)

	assert.Equal(t, statusDriveKindSharedFolder, status.Kind)
	assert.Equal(t, []statusStoreRoot{statusStoreRootSharedFolder}, querier.roots)
}

// Validates: R-2.10.4
//
// Status reads a configured drive's store as the drive's own root's, so a
// read failure at its root is told as the drive's.
func TestConfiguredDriveState_ReadsTheStoreAsADrive(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	querier := &rootRecordingQuerier{}

	configuredDriveState(t.Context(), driveid.MustCanonicalID("personal:owner@example.com"), &config.Drive{}, querier)

	assert.Equal(t, []statusStoreRoot{statusStoreRootDrive}, querier.roots)
}
