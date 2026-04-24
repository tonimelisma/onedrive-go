package cli

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func TestDescribeStatusCondition_CoversFamiliesAndFallback(t *testing.T) {
	t.Parallel()

	authPresentation := authstate.UnauthorizedIssuePresentation()
	cases := append(descriptorAuthAndRemoteCases(authPresentation), descriptorFilesystemCases()...)
	cases = append(cases, descriptorLocalRuntimeCases()...)
	cases = append(cases, descriptorFallbackCase())

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := describeStatusCondition(tc.key)
			assert.Equal(t, tc.wantTitle, got.Title)
			assert.Equal(t, tc.wantReason, got.Reason)
			assert.Equal(t, tc.wantAction, got.Action)
		})
	}
}

func TestBuildSyncStateInfo_DefaultsSamplingAndSorting(t *testing.T) {
	t.Parallel()

	snapshot := &syncengine.DriveStatusSnapshot{
		BaselineEntryCount: 11,
		RemoteDriftItems:   2,
		RetryingItems:      3,
		ObservationIssues: []syncengine.ObservationIssueRow{
			{
				Path:      "/z.txt",
				IssueType: syncengine.IssueInvalidFilename,
			},
			{
				Path:      "/y.txt",
				IssueType: syncengine.IssueInvalidFilename,
			},
		},
		BlockScopes: []*syncengine.BlockScope{
			{
				Key:           syncengine.SKPermRemoteWrite("Shared/A"),
				TrialInterval: time.Minute,
				NextTrialAt:   time.Unix(0, 0).UTC().Add(time.Minute),
			},
			{
				Key:           syncengine.SKPermRemoteWrite("Shared/B"),
				TrialInterval: time.Minute,
				NextTrialAt:   time.Unix(0, 0).UTC().Add(time.Minute),
			},
		},
		BlockedRetryWork: []syncengine.RetryWorkRow{
			{Path: "Shared/A/d.txt", ScopeKey: syncengine.SKPermRemoteWrite("Shared/A"), Blocked: true},
			{Path: "Shared/A/c.txt", ScopeKey: syncengine.SKPermRemoteWrite("Shared/A"), Blocked: true},
			{Path: "Shared/A/b.txt", ScopeKey: syncengine.SKPermRemoteWrite("Shared/A"), Blocked: true},
			{Path: "Shared/A/a.txt", ScopeKey: syncengine.SKPermRemoteWrite("Shared/A"), Blocked: true},
			{Path: "Shared/B/d.txt", ScopeKey: syncengine.SKPermRemoteWrite("Shared/B"), Blocked: true},
			{Path: "Shared/B/c.txt", ScopeKey: syncengine.SKPermRemoteWrite("Shared/B"), Blocked: true},
			{Path: "Shared/B/b.txt", ScopeKey: syncengine.SKPermRemoteWrite("Shared/B"), Blocked: true},
			{Path: "Shared/B/a.txt", ScopeKey: syncengine.SKPermRemoteWrite("Shared/B"), Blocked: true},
		},
	}

	info := buildSyncStateInfo(snapshot, false, 1)
	require.Len(t, info.Conditions, 3)
	assert.Equal(t, 11, info.FileCount)
	assert.Equal(t, 2, info.RemoteDrift)
	assert.Equal(t, 3, info.Retrying)
	assert.Equal(t, 10, info.ConditionCount)
	assert.Equal(t, 1, info.ExamplesLimit)
	assert.False(t, info.Verbose)

	assert.Equal(t, "SHARED FOLDER WRITES BLOCKED", info.Conditions[0].Title)
	assert.Equal(t, "Shared/A", info.Conditions[0].Scope)
	assert.Equal(t, statusScopeDirectory, info.Conditions[0].ScopeKind)
	assert.Equal(t, []string{"Shared/A/a.txt"}, info.Conditions[0].Paths)

	assert.Equal(t, "SHARED FOLDER WRITES BLOCKED", info.Conditions[1].Title)
	assert.Equal(t, "Shared/B", info.Conditions[1].Scope)
	assert.Equal(t, []string{"Shared/B/a.txt"}, info.Conditions[1].Paths)

	assert.Equal(t, "INVALID FILENAME", info.Conditions[2].Title)
	assert.Equal(t, []string{"/y.txt"}, info.Conditions[2].Paths)
}

// Validates: R-2.10.47
func TestBuildStatusConditionJSON_UsesCliOwnedPresentationBoundary(t *testing.T) {
	t.Parallel()

	groups := buildStatusConditionJSON(&syncengine.DriveStatusSnapshot{
		ObservationIssues: []syncengine.ObservationIssueRow{
			{Path: "/bad:name.txt", IssueType: syncengine.IssueInvalidFilename},
		},
		BlockScopes: []*syncengine.BlockScope{
			{
				Key:           syncengine.SKPermRemoteWrite("Shared/Docs"),
				TrialInterval: time.Minute,
				NextTrialAt:   time.Unix(0, 0).UTC().Add(time.Minute),
			},
		},
		BlockedRetryWork: []syncengine.RetryWorkRow{
			{Path: "Shared/Docs/a.txt", ScopeKey: syncengine.SKPermRemoteWrite("Shared/Docs"), Blocked: true},
			{Path: "Shared/Docs/b.txt", ScopeKey: syncengine.SKPermRemoteWrite("Shared/Docs"), Blocked: true},
			{Path: "Shared/Docs/c.txt", ScopeKey: syncengine.SKPermRemoteWrite("Shared/Docs"), Blocked: true},
		},
	}, false, 2)

	require.Len(t, groups, 2)
	assert.Equal(t, "SHARED FOLDER WRITES BLOCKED", groups[0].Title)
	assert.Equal(t, "Shared/Docs", groups[0].Scope)
	assert.Equal(t, statusScopeDirectory, groups[0].ScopeKind)
	assert.Equal(t, []string{"Shared/Docs/a.txt", "Shared/Docs/b.txt"}, groups[0].Paths)

	assert.Equal(t, "INVALID FILENAME", groups[1].Title)
	assert.Empty(t, groups[1].Scope)
	assert.Empty(t, groups[1].ScopeKind)
	assert.Equal(t, []string{"/bad:name.txt"}, groups[1].Paths)
}

func TestBuildSyncStateInfo_NilSnapshotUsesDefaults(t *testing.T) {
	t.Parallel()

	info := buildSyncStateInfo(nil, true, 0)
	assert.Zero(t, info.FileCount)
	assert.Zero(t, info.ConditionCount)
	assert.Equal(t, defaultVisiblePaths, info.ExamplesLimit)
	assert.True(t, info.Verbose)
	assert.Nil(t, info.Conditions)

	var buf bytes.Buffer
	require.NoError(t, printSyncStateText(&buf, "    ", &info, false))
	assert.Contains(t, buf.String(), "No active conditions.")
}

func TestStatusScopeKindFromScopeKey_CoversKinds(t *testing.T) {
	t.Parallel()

	assert.Empty(t, statusScopeKindFromScopeKey(syncengine.ScopeKey{}))
	assert.Equal(t, statusScopeDrive, statusScopeKindFromScopeKey(syncengine.SKThrottleDrive(driveid.New("drive-123"))))
	assert.Equal(t, statusScopeService, statusScopeKindFromScopeKey(syncengine.SKService()))
	assert.Equal(t, statusScopeDrive, statusScopeKindFromScopeKey(syncengine.SKQuotaOwn()))
	assert.Equal(t, statusScopeDirectory, statusScopeKindFromScopeKey(syncengine.SKPermRemoteWrite("Shared/Docs")))
	assert.Equal(t, statusScopeDirectory, statusScopeKindFromScopeKey(syncengine.SKPermLocalWrite("/tmp")))
	assert.Equal(t, statusScopeDisk, statusScopeKindFromScopeKey(syncengine.SKDiskLocal()))
	assert.Equal(t, statusScopeFile, statusScopeKindFromScopeKey(syncengine.ScopeKey{Kind: syncengine.ScopeKeyKind(99)}))
}

func TestSampleStrings_CoversVerboseAndTruncation(t *testing.T) {
	t.Parallel()

	values := []string{"a", "b", "c"}

	assert.Nil(t, sampleStrings(nil, false, 2))
	assert.Equal(t, values, sampleStrings(values, true, 1))
	assert.Equal(t, values[:2], sampleStrings(values, false, 2))
	assert.Equal(t, values, sampleStrings(values, false, 3))
}

// Validates: R-2.10.47
func TestSortStatusConditions_OrdersByCountThenConditionKeyThenScope(t *testing.T) {
	t.Parallel()

	groups := []statusConditionJSON{
		{ConditionKey: string(syncengine.ConditionInvalidFilename), Title: "INVALID FILENAME", Count: 1, Scope: "z"},
		{ConditionKey: string(syncengine.ConditionRemoteWriteDenied), Title: "SHARED FOLDER WRITES BLOCKED", Count: 2, Scope: "z"},
		{ConditionKey: string(syncengine.ConditionRemoteWriteDenied), Title: "SHARED FOLDER WRITES BLOCKED", Count: 2, Scope: "a"},
		{ConditionKey: string(syncengine.ConditionAuthenticationRequired), Title: "AUTHENTICATION REQUIRED", Count: 2, Scope: ""},
	}

	sortStatusConditions(groups)
	assert.Equal(t, []statusConditionJSON{
		{ConditionKey: string(syncengine.ConditionAuthenticationRequired), Title: "AUTHENTICATION REQUIRED", Count: 2, Scope: ""},
		{ConditionKey: string(syncengine.ConditionRemoteWriteDenied), Title: "SHARED FOLDER WRITES BLOCKED", Count: 2, Scope: "a"},
		{ConditionKey: string(syncengine.ConditionRemoteWriteDenied), Title: "SHARED FOLDER WRITES BLOCKED", Count: 2, Scope: "z"},
		{ConditionKey: string(syncengine.ConditionInvalidFilename), Title: "INVALID FILENAME", Count: 1, Scope: "z"},
	}, groups)
}

func TestProjectStoredConditionGroups_MergesScopeFamiliesAndDedupesPaths(t *testing.T) {
	t.Parallel()

	snapshot := &syncengine.DriveStatusSnapshot{
		ObservationIssues: []syncengine.ObservationIssueRow{
			{Path: "/bad:name.txt", IssueType: syncengine.IssueInvalidFilename},
		},
		BlockScopes: []*syncengine.BlockScope{
			{
				Key:           syncengine.SKPermRemoteWrite("Shared/Docs"),
				TrialInterval: time.Minute,
				NextTrialAt:   time.Unix(0, 0).UTC().Add(time.Minute),
			},
		},
		BlockedRetryWork: []syncengine.RetryWorkRow{
			{Path: "Shared/Docs/b.txt", ScopeKey: syncengine.SKPermRemoteWrite("Shared/Docs"), Blocked: true},
			{Path: "Shared/Docs/a.txt", ScopeKey: syncengine.SKPermRemoteWrite("Shared/Docs"), Blocked: true},
			{Path: "Shared/Docs/a.txt", ScopeKey: syncengine.SKPermRemoteWrite("Shared/Docs"), Blocked: true},
			{Path: "Shared/Docs/c.txt", ScopeKey: syncengine.SKPermRemoteWrite("Shared/Docs"), Blocked: true},
		},
	}

	groups := syncengine.ProjectStoredConditionGroups(snapshot)
	require.Len(t, groups, 2)

	assert.Equal(t, syncengine.ConditionRemoteWriteDenied, groups[0].ConditionKey)
	assert.Equal(t, syncengine.IssueRemoteWriteDenied, groups[0].ConditionType)
	assert.Equal(t, syncengine.SKPermRemoteWrite("Shared/Docs"), groups[0].ScopeKey)
	assert.Equal(t, 4, groups[0].Count)
	assert.Equal(t, []string{"Shared/Docs/a.txt", "Shared/Docs/b.txt", "Shared/Docs/c.txt"}, groups[0].Paths)

	assert.Equal(t, syncengine.ConditionInvalidFilename, groups[1].ConditionKey)
	assert.Equal(t, syncengine.IssueInvalidFilename, groups[1].ConditionType)
	assert.Equal(t, 1, groups[1].Count)
	assert.Equal(t, []string{"/bad:name.txt"}, groups[1].Paths)
}

func TestPrintConditionSection_NoActiveConditions(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printConditionSection(&buf, "    ", "      ", nil))
	assert.Equal(t, "    No active conditions.\n", buf.String())
}

func TestPrintConditionSection_RendersScopePathsAndNext(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := printConditionSection(&buf, "    ", "      ", []statusConditionJSON{
		{
			Title:  "RATE LIMITED",
			Reason: "OneDrive asked this remote location to slow down.",
			Action: "Wait for the retry window to expire (automatic retry in progress).",
			Scope:  "Drive A",
			Count:  3,
			Paths:  []string{"a", "b"},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, ""+
		"    RATE LIMITED (3 items)\n"+
		"      OneDrive asked this remote location to slow down. Wait for the retry window to expire (automatic retry in progress).\n"+
		"      Scope: Drive A\n"+
		"\n"+
		"      a\n"+
		"      b\n"+
		"      ... and 1 more (use --verbose to see all)\n"+
		"      Next: Wait for the retry window to expire (automatic retry in progress).\n",
		buf.String(),
	)
}

func TestPrintDriveSyncSections_WritesHeadingAndConditions(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := printMountSyncSections(&buf, "    ", &syncStateInfo{
		Conditions: []statusConditionJSON{
			{
				Title:  "INVALID FILENAME",
				Reason: "The filename contains characters not allowed by OneDrive.",
				Action: "Rename the file to remove invalid characters.",
				Count:  1,
				Paths:  []string{"/bad:name.txt"},
			},
		},
	}, false)
	require.NoError(t, err)

	assert.Equal(t, ""+
		"\n"+
		"    CONDITIONS\n"+
		"    INVALID FILENAME (1 item)\n"+
		"      The filename contains characters not allowed by OneDrive. Rename the file to remove invalid characters.\n"+
		"\n"+
		"      /bad:name.txt\n"+
		"      Next: Rename the file to remove invalid characters.\n",
		buf.String(),
	)
}

func TestPrintDriveSyncSections_NoConditionsUsesEmptyStateMessage(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printMountSyncSections(&buf, "    ", &syncStateInfo{}, true))
	assert.Equal(t, "\n    CONDITIONS\n    No active conditions.\n", buf.String())
}

func TestPrintConditionPaths_NoEllipsisAndNoPaths(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printConditionPaths(&buf, "      ", nil, 0))
	assert.Empty(t, buf.String())

	require.NoError(t, printConditionPaths(&buf, "      ", []string{"a", "b"}, 2))
	assert.Equal(t, "\n      a\n      b\n", buf.String())
}

func TestPrintAccountStatus_NilAndLeadingBlank(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printAccountStatus(&buf, nil, true, false))
	assert.Empty(t, buf.String())

	require.NoError(t, printAccountStatus(&buf, &statusAccount{
		Email:     "blank@example.com",
		DriveType: "personal",
		AuthState: authStateReady,
	}, true, false))
	assert.Equal(t, "\nAccount: blank@example.com [personal]\n  Auth:  ready\n", buf.String())
}

func TestPrintDriveStatus_WithoutSyncStateUsesSyncDirFallback(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printMountStatus(&buf, &statusMount{
		CanonicalID: "personal:blank@example.com",
		State:       driveStatePaused,
	}, false))

	assert.Equal(t, ""+
		"  personal:blank@example.com\n"+
		"    Sync dir:  (not set)\n"+
		"    State:     paused\n",
		buf.String(),
	)
}

func TestPrintStatusLiveDrives_RendersEntries(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printStatusLiveDrives(&buf, []statusLiveDrive{
		{ID: "drive-1", Name: "Docs", DriveType: "business", QuotaUsed: 1024, QuotaTotal: 2048},
		{ID: "drive-2", Name: "Photos", DriveType: "personal", QuotaUsed: 0, QuotaTotal: 0},
	}))

	output := buf.String()
	assert.Contains(t, output, "  Live drives:")
	assert.Contains(t, output, "    Docs (business)")
	assert.Contains(t, output, "      ID: drive-1")
	assert.Contains(t, output, "      Quota: 1.0 KB / 2.0 KB")
	assert.Contains(t, output, "    Photos (personal)")
	assert.Contains(t, output, "      ID: drive-2")
	assert.Contains(t, output, "      Quota: 0 B / 0 B")
}

func TestPrintSyncStateText_PerfOnlyOutput(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printSyncStateText(&buf, "    ", &syncStateInfo{
		PerfUnavailableReason: statusPerfUnavailableNoOwner,
	}, false))

	assert.Equal(t, ""+
		"\n"+
		"    PERF\n"+
		"    Live performance unavailable: "+statusPerfUnavailableNoOwner+"\n",
		buf.String(),
	)
}

func TestPrintSyncStateText_NilIsNoOp(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printSyncStateText(&buf, "    ", nil, false))
	assert.Empty(t, buf.String())
}

func TestPrintAccountStatus_RendersOptionalFieldsAndLiveDrive(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := printAccountStatus(&buf, &statusAccount{
		Email:          "alice@example.com",
		DriveType:      "business",
		UserID:         "user-123",
		AuthState:      authStateAuthenticationNeeded,
		AuthReason:     string(authReasonInvalidSavedLogin),
		AuthAction:     authAction(authReasonInvalidSavedLogin),
		DisplayName:    "Alice Example",
		OrgName:        "Contoso",
		DegradedReason: driveCatalogUnavailableReason,
		DegradedAction: degradedAction(driveCatalogUnavailableReason),
		LiveDrives: []statusLiveDrive{
			{ID: "drive-1", Name: "Work Files", DriveType: "business", QuotaUsed: 1024, QuotaTotal: 2048},
		},
		Mounts: []statusMount{
			{
				CanonicalID: "business:alice@example.com",
				DisplayName: "Documents",
				SyncDir:     "",
				State:       driveStateReady,
				SyncState: &syncStateInfo{
					FileCount:      7,
					ConditionCount: 1,
					RemoteDrift:    2,
					Retrying:       1,
					Conditions: []statusConditionJSON{
						{
							Title:  "INVALID FILENAME",
							Reason: "The filename contains characters not allowed by OneDrive.",
							Action: "Rename the file to remove invalid characters.",
							Count:  1,
							Paths:  []string{"/bad:name.txt"},
						},
					},
				},
			},
		},
	}, false, false)
	require.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, "Account: Alice Example (alice@example.com) [business]")
	assert.Contains(t, output, "  User ID: user-123")
	assert.Contains(t, output, "  Org:   Contoso")
	assert.Contains(t, output, "  Auth:  authentication_required")
	assert.Contains(t, output, "  Reason: The saved login for this account is invalid or unreadable.")
	assert.Contains(t, output, "  Action: Run 'onedrive-go login' to sign in.")
	assert.Contains(t, output, "  Live discovery: Couldn't finish loading live drive information for this account.")
	assert.Contains(t, output, "  Live action: "+degradedAction(driveCatalogUnavailableReason))
	assert.Contains(t, output, "  Live drives:")
	assert.Contains(t, output, "    Work Files (business)")
	assert.Contains(t, output, "      ID: drive-1")
	assert.Contains(t, output, "      Quota: 1.0 KB / 2.0 KB")
	assert.Contains(t, output, "  Documents (business:alice@example.com)")
	assert.Contains(t, output, "    Sync dir:  (not set)")
	assert.Contains(t, output, "    Files:     7")
	assert.Contains(t, output, "    Remote drift: 2 items")
	assert.Contains(t, output, "    Conditions: 1")
	assert.Contains(t, output, "    Retrying:  1 items")
}

func TestPrintStatusNextLine_EmptyHintProducesNoOutput(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printStatusNextLine(&buf, "      ", ""))
	assert.Empty(t, buf.String())
}

func TestPrintStatusText_NoAccountsPrintsSummaryOnly(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printStatusText(&buf, nil, false))
	assert.Equal(t, "Summary: 0 mounts, 0 conditions\n", buf.String())
}

func TestPrintStatusText_RendersMultiAccountSummary(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printStatusText(&buf, []statusAccount{
		{
			Email:     "ready@example.com",
			DriveType: "personal",
			AuthState: authStateReady,
			Mounts: []statusMount{
				{
					CanonicalID: "personal:ready@example.com",
					State:       driveStateReady,
					SyncDir:     "/sync/ready",
					SyncState: &syncStateInfo{
						FileCount:      5,
						ConditionCount: 2,
						RemoteDrift:    1,
						Retrying:       1,
					},
				},
			},
		},
		{
			Email:      "needs-auth@example.com",
			DriveType:  "business",
			AuthState:  authStateAuthenticationNeeded,
			AuthReason: string(authReasonMissingLogin),
			AuthAction: authAction(authReasonMissingLogin),
			Mounts: []statusMount{
				{
					CanonicalID: "business:needs-auth@example.com",
					State:       driveStatePaused,
					SyncDir:     "/sync/paused",
				},
			},
		},
	}, false))

	output := buf.String()
	assert.Contains(t, output, "Summary: 2 mounts (1 ready, 1 paused, 1 accounts requiring auth), 2 conditions, 1 remote drift, 1 retrying")
	assert.Contains(t, output, "Account: ready@example.com [personal]")
	assert.Contains(t, output, "  personal:ready@example.com")
	assert.Contains(t, output, "    Sync dir:  /sync/ready")
	assert.Contains(t, output, "Account: needs-auth@example.com [business]")
	assert.Contains(t, output, "  Auth:  authentication_required")
	assert.Contains(t, output, "  Reason: No saved login was found for this account.")
	assert.Contains(t, output, "  Action: Run 'onedrive-go login' to sign in.")
	assert.Contains(t, output, "  business:needs-auth@example.com")
	assert.Contains(t, output, "    State:     paused")
	parts := bytes.Split([]byte(output), []byte("Account: "))
	require.GreaterOrEqual(t, len(parts), 3)
}

type descriptorCase struct {
	name       string
	key        syncengine.ConditionKey
	wantTitle  string
	wantReason string
	wantAction string
}

func descriptorAuthAndRemoteCases(authPresentation authstate.Presentation) []descriptorCase {
	return []descriptorCase{
		{
			name:       "authentication required",
			key:        syncengine.ConditionAuthenticationRequired,
			wantTitle:  "AUTHENTICATION REQUIRED",
			wantReason: authPresentation.Reason,
			wantAction: authPresentation.Action,
		},
		{
			name:       "quota exceeded",
			key:        syncengine.ConditionQuotaExceeded,
			wantTitle:  "QUOTA EXCEEDED",
			wantReason: "The OneDrive storage quota for this sync scope is full.",
			wantAction: "Free up space in the owning drive, or ask the shared-folder owner to do so.",
		},
		{
			name:       "service outage",
			key:        syncengine.ConditionServiceOutage,
			wantTitle:  "SERVICE OUTAGE",
			wantReason: "OneDrive service is temporarily unavailable.",
			wantAction: "Wait for the service to recover (automatic retry in progress).",
		},
		{
			name:       "rate limited",
			key:        syncengine.ConditionRateLimited,
			wantTitle:  "RATE LIMITED",
			wantReason: "OneDrive asked this remote location to slow down.",
			wantAction: "Wait for the retry window to expire (automatic retry in progress).",
		},
		{
			name:       "remote write denied",
			key:        syncengine.ConditionRemoteWriteDenied,
			wantTitle:  "SHARED FOLDER WRITES BLOCKED",
			wantReason: "This shared folder is read-only for your current write attempts. Downloads continue normally.",
			wantAction: "Remove or ignore local write changes here, or ask the owner for edit permissions if the write was intended.",
		},
		{
			name:       "remote read denied",
			key:        syncengine.ConditionRemoteReadDenied,
			wantTitle:  "REMOTE READ BLOCKED",
			wantReason: "This remote content can no longer be downloaded with your current permissions.",
			wantAction: "Restore access to the shared item, or remove the blocked content from this sync scope.",
		},
	}
}

func descriptorFilesystemCases() []descriptorCase {
	return []descriptorCase{
		{
			name:       "local read denied",
			key:        syncengine.ConditionLocalReadDenied,
			wantTitle:  "LOCAL READ BLOCKED",
			wantReason: "The local source file or directory can no longer be read.",
			wantAction: "Restore local read access so uploads and conflict recovery can read the source content.",
		},
		{
			name:       "local write denied",
			key:        syncengine.ConditionLocalWriteDenied,
			wantTitle:  "LOCAL WRITE BLOCKED",
			wantReason: "The local destination path can no longer be created, renamed, or updated.",
			wantAction: "Restore local write access so downloads and local filesystem updates can complete.",
		},
		{
			name:       "invalid filename",
			key:        syncengine.ConditionInvalidFilename,
			wantTitle:  "INVALID FILENAME",
			wantReason: "The filename contains characters not allowed by OneDrive.",
			wantAction: "Rename the file to remove invalid characters.",
		},
		{
			name:       "path too long",
			key:        syncengine.ConditionPathTooLong,
			wantTitle:  "PATH TOO LONG",
			wantReason: "The full path exceeds OneDrive's 400-character limit.",
			wantAction: "Shorten the path by renaming files or folders.",
		},
		{
			name:       "file too large",
			key:        syncengine.ConditionFileTooLarge,
			wantTitle:  "FILE TOO LARGE",
			wantReason: "The file exceeds the maximum upload size.",
			wantAction: "Reduce the file size or move it out of the sync dir.",
		},
		{
			name:       "case collision",
			key:        syncengine.ConditionCaseCollision,
			wantTitle:  "CASE COLLISION",
			wantReason: "Two files differ only in letter case, which OneDrive cannot distinguish.",
			wantAction: "Rename one of the conflicting files.",
		},
	}
}

func descriptorLocalRuntimeCases() []descriptorCase {
	return []descriptorCase{
		{
			name:       "disk full",
			key:        syncengine.ConditionDiskFull,
			wantTitle:  "DISK FULL",
			wantReason: "Local disk space is insufficient for downloads.",
			wantAction: "Free up local disk space.",
		},
		{
			name:       "hash error",
			key:        syncengine.ConditionHashError,
			wantTitle:  "HASH ERROR",
			wantReason: "File hashing failed unexpectedly.",
			wantAction: "Check file integrity and retry.",
		},
		{
			name:       "file too large for space",
			key:        syncengine.ConditionFileTooLargeForSpace,
			wantTitle:  "FILE TOO LARGE FOR SPACE",
			wantReason: "The file is larger than available local disk space.",
			wantAction: "Free up local disk space to fit this file.",
		},
	}
}

func descriptorFallbackCase() descriptorCase {
	return descriptorCase{
		name:       "unexpected fallback",
		key:        syncengine.ConditionKey("custom_condition"),
		wantTitle:  "SYNC CONDITION",
		wantReason: "An unexpected sync condition needs attention.",
		wantAction: "Check logs for details or rerun status after the next sync pass.",
	}
}
