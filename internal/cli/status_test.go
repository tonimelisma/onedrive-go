package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	_ "modernc.org/sqlite"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

// mockNameReader returns fixed display name and org name for testing.
type mockNameReader struct {
	displayName string
	orgName     string
}

func (m *mockNameReader) ReadAccountNames(_ string, _ []driveid.CanonicalID) (string, string) {
	return m.displayName, m.orgName
}

// mockSavedLoginChecker returns a fixed auth-health state for all accounts.
type mockSavedLoginChecker struct {
	reason authstate.Reason
}

func (m *mockSavedLoginChecker) CheckAccountAuth(_ context.Context, _ string, _ []driveid.CanonicalID) accountAuthHealth {
	switch m.reason {
	case authReasonMissingLogin:
		return accountAuthHealth{
			State:  authStateAuthenticationNeeded,
			Reason: authReasonMissingLogin,
			Action: authAction(authReasonMissingLogin),
		}
	case authReasonInvalidSavedLogin:
		return accountAuthHealth{
			State:  authStateAuthenticationNeeded,
			Reason: authReasonInvalidSavedLogin,
			Action: authAction(authReasonInvalidSavedLogin),
		}
	case authReasonSyncAuthRejected:
		return accountAuthHealth{
			State:  authStateAuthenticationNeeded,
			Reason: authReasonSyncAuthRejected,
			Action: authAction(authReasonSyncAuthRejected),
		}
	default:
		return accountAuthHealth{State: authStateReady}
	}
}

// mockSyncStateQuerier returns a fixed sync state for all drives.
type mockSyncStateQuerier struct {
	state *syncStateInfo
}

func (m *mockSyncStateQuerier) QuerySyncState(_ string) *syncStateInfo {
	return m.state
}

func seedObservationIssueForStatusTest(
	t *testing.T,
	store *syncengine.SyncStore,
	issue *syncengine.ObservationIssue,
) {
	t.Helper()

	require.NoError(t, store.ReconcileObservationFindings(
		t.Context(),
		&syncengine.ObservationFindingsBatch{
			Issues:            []syncengine.ObservationIssue{*issue},
			ManagedIssueTypes: []string{issue.IssueType},
			ManagedPaths:      []string{issue.Path},
		},
		time.Now().UTC(),
	))
}

func testShortcutStatusChild(
	parentCID driveid.CanonicalID,
	state syncengine.ShortcutRootState,
) childMountStatusInput {
	const (
		bindingID    = "binding-docs"
		relativePath = "Shortcuts/Docs"
	)
	alias := filepath.Base(relativePath)
	return childMountStatusInput{
		ParentID: parentCID,
		Root: syncengine.ShortcutRootRecord{
			NamespaceID:       parentCID.String(),
			BindingItemID:     bindingID,
			RelativeLocalPath: relativePath,
			LocalAlias:        alias,
			RemoteDriveID:     driveid.New("remote-drive"),
			RemoteItemID:      "remote-root",
			RemoteIsFolder:    true,
			State:             state,
			ProtectedPaths:    []string{relativePath},
		},
	}
}

func TestDriveState_Ready(t *testing.T) {
	d := &config.Drive{}
	assert.Equal(t, "ready", driveState(d))
}

func TestDriveState_Paused(t *testing.T) {
	paused := true
	d := &config.Drive{Paused: &paused}
	assert.Equal(t, "paused", driveState(d))
}

func TestDriveState_PausedOverridesNoToken(t *testing.T) {
	// Paused remains an explicit operational state regardless of auth health.
	paused := true
	d := &config.Drive{Paused: &paused}
	assert.Equal(t, "paused", driveState(d))
}

// Validates: R-2.10.1
func TestBuildChildStatusMount_InheritsParentPause(t *testing.T) {
	parentCID := driveid.MustCanonicalID("personal:alice@example.com")
	child := testShortcutStatusChild(parentCID, syncengine.ShortcutRootStateActive)
	paused := true
	mount := buildChildStatusMount(
		config.Drive{SyncDir: "/tmp/sync-root", Paused: &paused},
		&child,
		nil,
	)

	assert.Equal(t, driveStatePaused, mount.State)
}

type shortcutStatusLifecycleCase struct {
	name               string
	state              syncengine.ShortcutRootState
	displayState       string
	reason             string
	detail             string
	action             string
	autoRetry          *bool
	waitingReplacement string
}

func shortcutStatusLifecycleCases() []shortcutStatusLifecycleCase {
	return []shortcutStatusLifecycleCase{
		{
			name:         "active",
			state:        syncengine.ShortcutRootStateActive,
			displayState: driveStateReady,
		},
		{
			name:         "target_unavailable",
			state:        syncengine.ShortcutRootStateTargetUnavailable,
			displayState: string(syncengine.ShortcutRootStateTargetUnavailable),
			reason:       string(syncengine.ShortcutRootStateTargetUnavailable),
			detail:       "The shortcut target is unavailable.",
			action:       "Restore target access or remove the shortcut alias.",
			autoRetry:    boolPtr(true),
		},
		{
			name:         "blocked_path",
			state:        syncengine.ShortcutRootStateBlockedPath,
			displayState: string(syncengine.ShortcutRootStateBlockedPath),
			reason:       string(syncengine.ShortcutRootStateBlockedPath),
			detail:       "The shortcut alias path is blocked.",
			action:       "Clear the blocking local path.",
			autoRetry:    boolPtr(true),
		},
		{
			name:         "rename_ambiguous",
			state:        syncengine.ShortcutRootStateRenameAmbiguous,
			displayState: string(syncengine.ShortcutRootStateRenameAmbiguous),
			reason:       string(syncengine.ShortcutRootStateRenameAmbiguous),
			detail:       "Multiple same-folder shortcut alias rename candidates were found.",
			action:       "Keep exactly one renamed shortcut alias or restore the original name.",
			autoRetry:    boolPtr(true),
		},
		{
			name:         "alias_mutation_blocked",
			state:        syncengine.ShortcutRootStateAliasMutationBlocked,
			displayState: string(syncengine.ShortcutRootStateAliasMutationBlocked),
			reason:       string(syncengine.ShortcutRootStateAliasMutationBlocked),
			detail:       "The parent engine cannot update the shortcut alias in OneDrive.",
			action:       "Fix account, network, or permission access, or restore the local alias.",
			autoRetry:    boolPtr(true),
		},
		{
			name:         "removed_final_drain",
			state:        syncengine.ShortcutRootStateRemovedFinalDrain,
			displayState: "pending_removal",
			reason:       string(syncengine.ShortcutRootStateRemovedFinalDrain),
			detail:       "The shortcut alias was removed; child sync is finishing before release.",
			autoRetry:    boolPtr(true),
		},
		{
			name:         "removed_cleanup_blocked",
			state:        syncengine.ShortcutRootStateRemovedCleanupBlocked,
			displayState: "pending_removal",
			reason:       string(syncengine.ShortcutRootStateRemovedCleanupBlocked),
			detail:       "The parent engine cannot release the protected shortcut alias path.",
			action:       "Clear the local filesystem blocker.",
			autoRetry:    boolPtr(true),
		},
		{
			name:         "removed_release_pending",
			state:        syncengine.ShortcutRootStateRemovedReleasePending,
			displayState: "pending_removal",
			reason:       string(syncengine.ShortcutRootStateRemovedReleasePending),
			detail:       "Child sync finished; the parent engine is releasing the protected shortcut alias path.",
			autoRetry:    boolPtr(true),
		},
		{
			name:               "same_path_replacement_waiting",
			state:              syncengine.ShortcutRootStateSamePathReplacementWaiting,
			displayState:       "pending_removal",
			reason:             string(syncengine.ShortcutRootStateSamePathReplacementWaiting),
			detail:             "A new shortcut is waiting for the old child sync to finish.",
			autoRetry:          boolPtr(true),
			waitingReplacement: "Shortcuts/Docs",
		},
		{
			name:         "duplicate_target",
			state:        syncengine.ShortcutRootStateDuplicateTarget,
			displayState: string(syncengine.ShortcutRootStateDuplicateTarget),
			reason:       string(syncengine.ShortcutRootStateDuplicateTarget),
			detail:       "Another shortcut alias in this parent already projects the same target.",
			action:       "Remove or rename the duplicate shortcut alias.",
			autoRetry:    boolPtr(true),
		},
	}
}

// Validates: R-2.10.1
func TestBuildChildStatusMount_RendersLifecycleState(t *testing.T) {
	parentCID := driveid.MustCanonicalID("personal:alice@example.com")
	tests := shortcutStatusLifecycleCases()
	for i := range tests {
		t.Run(tests[i].name, func(t *testing.T) {
			tc := tests[i]
			mount := buildStatusLifecycleMount(t, parentCID, tc)
			assertStatusLifecycleMount(t, &mount, tc)
			assertStatusLifecycleText(t, &mount, tc)
			assertStatusLifecycleJSON(t, &mount, tc)
		})
	}
}

func buildStatusLifecycleMount(
	t *testing.T,
	parentCID driveid.CanonicalID,
	tc shortcutStatusLifecycleCase,
) statusMount {
	t.Helper()

	child := testShortcutStatusChild(parentCID, tc.state)
	if tc.waitingReplacement != "" {
		child.Root.Waiting = &syncengine.ShortcutRootReplacement{
			BindingItemID:     "binding-new",
			RelativeLocalPath: tc.waitingReplacement,
			LocalAlias:        "Docs",
			RemoteDriveID:     driveid.New("remote-drive-new"),
			RemoteItemID:      "remote-root-new",
			RemoteIsFolder:    true,
		}
	}
	return buildChildStatusMount(config.Drive{SyncDir: "/tmp/sync-root"}, &child, nil)
}

func assertStatusLifecycleMount(t *testing.T, mount *statusMount, tc shortcutStatusLifecycleCase) {
	t.Helper()

	assert.Equal(t, tc.displayState, mount.State)
	assert.Equal(t, tc.reason, mount.StateReason)
	assert.Equal(t, tc.detail, mount.StateDetail)
	assert.Equal(t, tc.action, mount.RecoveryAction)
	assert.Equal(t, tc.waitingReplacement, mount.WaitingReplacement)
	if tc.autoRetry == nil {
		assert.Nil(t, mount.AutoRetry)
		assert.Empty(t, mount.ProtectedCurrentPath)
		return
	}
	require.NotNil(t, mount.AutoRetry)
	assert.Equal(t, *tc.autoRetry, *mount.AutoRetry)
	assert.Equal(t, "/tmp/sync-root/Shortcuts/Docs", mount.ProtectedCurrentPath)
}

func assertStatusLifecycleText(t *testing.T, mount *statusMount, tc shortcutStatusLifecycleCase) {
	t.Helper()

	var text bytes.Buffer
	require.NoError(t, printMountStatus(&text, mount, false))
	rendered := text.String()
	assert.NotContains(t, rendered, "onedrive-go ")
	assert.NotContains(t, rendered, "Run ")
	if tc.reason != "" {
		assert.Contains(t, rendered, "Reason:    "+tc.reason)
		assert.Contains(t, rendered, "Next:      "+tc.detail)
		assert.Contains(t, rendered, "Protected current path: /tmp/sync-root/Shortcuts/Docs")
		assert.Contains(t, rendered, "Auto retry: yes")
	}
	if tc.action != "" {
		assert.Contains(t, rendered, "Action:    "+tc.action)
	}
	if tc.waitingReplacement != "" {
		assert.Contains(t, rendered, "Waiting replacement: "+tc.waitingReplacement)
	}
}

func assertStatusLifecycleJSON(t *testing.T, mount *statusMount, tc shortcutStatusLifecycleCase) {
	t.Helper()

	encoded, err := json.Marshal(mount)
	require.NoError(t, err)
	jsonText := string(encoded)
	if tc.reason == "" {
		assert.NotContains(t, jsonText, "state_reason")
		assert.NotContains(t, jsonText, "auto_retry")
		return
	}
	assert.Contains(t, jsonText, `"state_reason":"`+tc.reason+`"`)
	assert.Contains(t, jsonText, `"state_detail":`)
	assert.Contains(t, jsonText, `"protected_current_path":`)
	assert.Contains(t, jsonText, `"auto_retry":true`)
}

func boolPtr(value bool) *bool {
	return &value
}

// Validates: R-2.3.3, R-2.4.8, R-2.10.4
func TestBuildChildStatusMount_SurfacesProtectedPaths(t *testing.T) {
	parentCID := driveid.MustCanonicalID("personal:alice@example.com")
	child := testShortcutStatusChild(parentCID, syncengine.ShortcutRootStateRemovedFinalDrain)
	child.Root.ProtectedPaths = []string{"Shortcuts/Docs", "Shortcuts/Old Docs"}
	mount := buildChildStatusMount(
		config.Drive{SyncDir: "/tmp/sync-root"},
		&child,
		nil,
	)

	assert.Equal(t, "/tmp/sync-root/Shortcuts/Docs", mount.ProtectedCurrentPath)
	assert.Equal(t, []string{"/tmp/sync-root/Shortcuts/Old Docs"}, mount.ProtectedReservedPaths)
	assert.Contains(t, mount.StateDetail, "child sync")
	assert.NotContains(t, mount.RecoveryAction, "rerun sync")
}

// Validates: R-2.10.1
func TestBuildSingleAccountStatusWith_NestsChildMountsUnderParent(t *testing.T) {
	parentCID := driveid.MustCanonicalID("personal:alice@example.com")
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			parentCID: {SyncDir: "/tmp/sync-root"},
		},
	}
	children := map[driveid.CanonicalID][]childMountStatusInput{
		parentCID: {testShortcutStatusChild(parentCID, syncengine.ShortcutRootStateActive)},
	}

	account := buildSingleAccountStatusWith(
		cfg,
		"alice@example.com",
		[]driveid.CanonicalID{parentCID},
		children,
		&mockNameReader{},
		&mockSavedLoginChecker{},
		nil,
	)

	require.Len(t, account.Mounts, 1)
	parent := account.Mounts[0]
	assert.Equal(t, parentCID.String(), parent.CanonicalID)
	require.Len(t, parent.ChildMounts, 1)
	assert.Equal(t, config.ChildMountID(parentCID.String(), "binding-docs"), parent.ChildMounts[0].MountID)
	assert.Empty(t, parent.ChildMounts[0].CanonicalID)
}

// Validates: R-2.10.1
func TestPrintMountStatus_RendersChildLifecycleReasonAndNextAction(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := printMountStatus(&buf, &statusMount{
		MountID:        "child-docs",
		NamespaceID:    "personal:alice@example.com",
		ProjectionKind: statusProjectionChild,
		DisplayName:    "Docs",
		SyncDir:        "/tmp/sync-root/Shortcuts/Docs",
		State:          string(syncengine.ShortcutRootStateTargetUnavailable),
		StateReason:    string(syncengine.ShortcutRootStateTargetUnavailable),
		StateDetail:    "The shortcut target is unavailable.",
	}, false)
	require.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, "Reason:    "+string(syncengine.ShortcutRootStateTargetUnavailable))
	assert.Contains(t, output, "Next:      The shortcut target is unavailable.")
	assert.Contains(t, output, "Control:   Parent drive pause/resume and the OneDrive shortcut")
}

// Validates: R-2.3.3, R-2.4.8, R-2.10.4
func TestPrintMountStatus_RendersGuidedShortcutRecovery(t *testing.T) {
	t.Parallel()

	autoRetry := true
	var buf bytes.Buffer
	err := printMountStatus(&buf, &statusMount{
		MountID:                "child-docs",
		NamespaceID:            "personal:alice@example.com",
		ProjectionKind:         statusProjectionChild,
		DisplayName:            "Docs",
		SyncDir:                "/tmp/sync-root/Shortcuts/Docs",
		State:                  "pending_removal",
		StateReason:            string(syncengine.ShortcutRootStateRemovedFinalDrain),
		StateDetail:            "The shortcut alias was removed; child sync is finishing before release.",
		ProtectedCurrentPath:   "/tmp/sync-root/Shortcuts/Docs",
		ProtectedReservedPaths: []string{"/tmp/sync-root/Shortcuts/Old Docs"},
		AutoRetry:              &autoRetry,
	}, false)
	require.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, "Protected current path: /tmp/sync-root/Shortcuts/Docs")
	assert.Contains(t, output, "Reserved path: /tmp/sync-root/Shortcuts/Old Docs")
	assert.Contains(t, output, "Next:      The shortcut alias was removed")
	assert.Contains(t, output, "Auto retry: yes")
}

// Validates: R-2.10.1
func TestBuildChildStatusMount_UsesMountIDWithoutSyntheticSharedCanonical(t *testing.T) {
	parentCID := driveid.MustCanonicalID("personal:alice@example.com")
	child := testShortcutStatusChild(parentCID, syncengine.ShortcutRootStateBlockedPath)
	child.Root.BlockedDetail = "content root already projected by standalone mount"
	mount := buildChildStatusMount(
		config.Drive{SyncDir: "/tmp/sync-root"},
		&child,
		nil,
	)

	assert.Equal(t, config.ChildMountID(parentCID.String(), "binding-docs"), mount.MountID)
	assert.Equal(t, statusProjectionChild, mount.ProjectionKind)
	assert.Empty(t, mount.CanonicalID)
	assert.Equal(t, "Docs ("+mount.MountID+")", statusMountLabel(&mount))
	assert.Equal(t, string(syncengine.ShortcutRootStateBlockedPath), mount.StateReason)
	assert.Contains(t, mount.StateDetail, "standalone mount")
	assert.Contains(t, mount.RecoveryAction, "Clear the blocking local path")
	require.NotNil(t, mount.AutoRetry)
	assert.True(t, *mount.AutoRetry)
	assert.NotContains(t, mount.StateDetail, "pause")

	encoded, err := json.Marshal(mount)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"mount_id":"`+mount.MountID+`"`)
	assert.Contains(t, string(encoded), `"namespace_id":"personal:alice@example.com"`)
	assert.Contains(t, string(encoded), `"state_reason":"blocked_path"`)
	assert.Contains(t, string(encoded), `"state_detail":`)
	assert.Contains(t, string(encoded), `"recovery_action":`)
	assert.Contains(t, string(encoded), `"auto_retry":true`)
	assert.NotContains(t, string(encoded), "canonical_id")
	assert.NotContains(t, string(encoded), "shared:")
}

func TestGroupDrivesByAccount(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"):   {},
			driveid.MustCanonicalID("business:alice@example.com"):   {},
			driveid.MustCanonicalID("personal:bob@example.com"):     {},
			driveid.MustCanonicalID("business:charlie@example.com"): {},
		},
	}

	grouped, order := groupDrivesByAccount(cfg)

	// Order should be sorted alphabetically.
	assert.Len(t, order, 3)
	assert.Equal(t, "alice@example.com", order[0])
	assert.Equal(t, "bob@example.com", order[1])
	assert.Equal(t, "charlie@example.com", order[2])

	// alice has 2 drives.
	assert.Len(t, grouped["alice@example.com"], 2)
	assert.Len(t, grouped["bob@example.com"], 1)
	assert.Len(t, grouped["charlie@example.com"], 1)
}

func TestGroupDrivesByAccount_WithSharePoint(t *testing.T) {
	// With typed CanonicalID keys, SharePoint drives are grouped
	// under the same account as personal/business drives via .Email().
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("business:alice@contoso.com"):                    {},
			driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"):   {},
			driveid.MustCanonicalID("sharepoint:alice@contoso.com:engineering:Wiki"): {},
		},
	}

	grouped, order := groupDrivesByAccount(cfg)

	// All three drives belong to alice@contoso.com.
	assert.Len(t, order, 1)
	assert.Equal(t, "alice@contoso.com", order[0])
	assert.Len(t, grouped["alice@contoso.com"], 3)
}

func TestGroupDrivesByAccount_Empty(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{},
	}

	grouped, order := groupDrivesByAccount(cfg)

	assert.Empty(t, order)
	assert.Empty(t, grouped)
}

// Validates: R-6.3.2
func TestNewStatusCmd_Structure(t *testing.T) {
	cmd := newStatusCmd()
	assert.Equal(t, "status", cmd.Name())
	assert.NotEmpty(t, cmd.Short)
	assert.NotNil(t, cmd.RunE)
	assert.Contains(t, cmd.Short, "sync status")
	assert.Contains(t, cmd.Long, "same per-mount sync-health contract")
	assert.Contains(t, cmd.Long, "--drive to filter")
	assert.Contains(t, cmd.Long, "--verbose")
}

// --- buildStatusAccountsWith tests (B-036) ---

func TestBuildStatusAccountsWith_SingleAccount(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"): {SyncDir: "~/OneDrive"},
		},
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockNameReader{displayName: "Alice", orgName: ""},
		&mockSavedLoginChecker{},
		&mockSyncStateQuerier{},
	)

	require.Len(t, accounts, 1)
	acct := accounts[0]
	assert.Equal(t, "alice@example.com", acct.Email)
	assert.Equal(t, "personal", acct.DriveType)
	assert.Equal(t, authStateReady, acct.AuthState)
	assert.Equal(t, "Alice", acct.DisplayName)

	require.Len(t, acct.Mounts, 1)
	assert.Equal(t, "~/OneDrive", acct.Mounts[0].SyncDir)
	assert.Equal(t, driveStateReady, acct.Mounts[0].State)
}

func TestBuildStatusAccountsWith_MultiAccountGrouping(t *testing.T) {
	paused := true
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"):   {SyncDir: "~/OneDrive"},
			driveid.MustCanonicalID("business:alice@example.com"):   {SyncDir: "~/Work"},
			driveid.MustCanonicalID("personal:bob@example.com"):     {SyncDir: "~/Bob", Paused: &paused},
			driveid.MustCanonicalID("business:charlie@example.com"): {SyncDir: "~/Charlie"},
		},
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockNameReader{displayName: "", orgName: ""},
		&mockSavedLoginChecker{},
		&mockSyncStateQuerier{},
	)

	require.Len(t, accounts, 3)

	// Sorted alphabetically by email.
	assert.Equal(t, "alice@example.com", accounts[0].Email)
	assert.Len(t, accounts[0].Mounts, 2)

	assert.Equal(t, "bob@example.com", accounts[1].Email)
	assert.Len(t, accounts[1].Mounts, 1)
	assert.Equal(t, driveStatePaused, accounts[1].Mounts[0].State)

	assert.Equal(t, "charlie@example.com", accounts[2].Email)
}

// Validates: R-2.10.47
func TestBuildStatusAccountsWith_AuthenticationRequiredStates(t *testing.T) {
	tests := []struct {
		name       string
		savedLogin authstate.Reason
		wantReason string
	}{
		{
			name:       "missing token",
			savedLogin: authReasonMissingLogin,
			wantReason: string(authReasonMissingLogin),
		},
		{
			name:       "invalid saved login",
			savedLogin: authReasonInvalidSavedLogin,
			wantReason: string(authReasonInvalidSavedLogin),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				Drives: map[driveid.CanonicalID]config.Drive{
					driveid.MustCanonicalID("personal:alice@example.com"): {SyncDir: "~/OneDrive"},
				},
			}

			accounts := buildStatusAccountsWith(cfg,
				&mockNameReader{},
				&mockSavedLoginChecker{reason: tc.savedLogin},
				&mockSyncStateQuerier{},
			)

			require.Len(t, accounts, 1)
			assert.Equal(t, authStateAuthenticationNeeded, accounts[0].AuthState)
			assert.Equal(t, tc.wantReason, accounts[0].AuthReason)
			assert.Equal(t, driveStateReady, accounts[0].Mounts[0].State)
		})
	}
}

func TestBuildStatusAccountsWith_EmptySyncDir(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"): {SyncDir: ""},
		},
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockNameReader{},
		&mockSavedLoginChecker{},
		&mockSyncStateQuerier{},
	)

	require.Len(t, accounts, 1)
	// Empty sync_dir no longer overrides drive state — drive is still "ready".
	assert.Equal(t, driveStateReady, accounts[0].Mounts[0].State)
}

func TestBuildStatusAccountsWith_SharePointGrouping(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("business:alice@contoso.com"):                    {SyncDir: "~/Work"},
			driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"):   {SyncDir: "~/Marketing"},
			driveid.MustCanonicalID("sharepoint:alice@contoso.com:engineering:Wiki"): {SyncDir: "~/Eng"},
		},
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockNameReader{displayName: "Alice", orgName: "Contoso"},
		&mockSavedLoginChecker{},
		&mockSyncStateQuerier{},
	)

	require.Len(t, accounts, 1)
	acct := accounts[0]
	assert.Equal(t, "alice@contoso.com", acct.Email)
	assert.Equal(t, "business", acct.DriveType) // business preferred over sharepoint
	assert.Equal(t, "Contoso", acct.OrgName)
	assert.Len(t, acct.Mounts, 3)
}

func TestReadAccountMeta_UsesProfileFieldOrder(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cid := driveid.MustCanonicalID("personal:alice@example.com")
	seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
		account.DisplayName = authTestDisplayNameAlice
		account.OrgName = workflowTestOrgContoso
		account.UserID = authTestUserID1
		account.PrimaryDriveID = workflowTestDriveID
	})

	displayName, orgName := readAccountMeta("alice@example.com", []driveid.CanonicalID{cid}, slog.New(slog.DiscardHandler))
	assert.Equal(t, "Alice", displayName)
	assert.Equal(t, "Contoso", orgName)
}

func TestReadAccountMeta_FallsBackToTokenProbe(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cid := driveid.MustCanonicalID("personal:alice@example.com")
	seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
		account.DisplayName = authTestDisplayNameAlice
		account.OrgName = workflowTestOrgContoso
		account.UserID = authTestUserID1
		account.PrimaryDriveID = workflowTestDriveID
	})

	tokenPath := config.DriveTokenPath(cid)
	require.NoError(t, os.MkdirAll(filepath.Dir(tokenPath), 0o700))
	require.NoError(t, os.WriteFile(tokenPath, []byte("{}"), 0o600))

	displayName, orgName := readAccountMeta("alice@example.com", nil, slog.New(slog.DiscardHandler))
	assert.Equal(t, "Alice", displayName)
	assert.Equal(t, "Contoso", orgName)
}

func TestInspectSavedLogin_MissingTokenUsesFallback(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	state := inspectSavedLogin(t.Context(), "missing@example.com", nil, slog.New(slog.DiscardHandler))
	assert.Equal(t, authReasonMissingLogin, state)
}

func TestInspectSavedLogin_ValidToken(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cid := driveid.MustCanonicalID("personal:alice@example.com")
	tokenPath := config.DriveTokenPath(cid)
	require.NoError(t, os.MkdirAll(filepath.Dir(tokenPath), 0o700))
	require.NoError(t, tokenfile.Save(tokenPath, &oauth2.Token{
		AccessToken:  "access",
		RefreshToken: "refresh",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
	}))

	state := inspectSavedLogin(t.Context(), "alice@example.com", []driveid.CanonicalID{cid}, slog.New(slog.DiscardHandler))
	assert.Empty(t, state)
}

func TestInspectSavedLogin_InvalidTokenFileReturnsInvalidSavedLogin(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cid := driveid.MustCanonicalID("personal:alice@example.com")
	tokenPath := config.DriveTokenPath(cid)
	require.NoError(t, os.MkdirAll(filepath.Dir(tokenPath), 0o700))
	require.NoError(t, os.WriteFile(tokenPath, []byte("{invalid-json"), 0o600))

	state := inspectSavedLogin(t.Context(), "alice@example.com", []driveid.CanonicalID{cid}, slog.New(slog.DiscardHandler))
	assert.Equal(t, authReasonInvalidSavedLogin, state)
}

func TestBuildStatusAccountsWith_DisplayNameFromConfig(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"): {
				SyncDir:     "~/OneDrive",
				DisplayName: "My Home Drive",
			},
		},
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockNameReader{},
		&mockSavedLoginChecker{},
		&mockSyncStateQuerier{},
	)

	require.Len(t, accounts, 1)
	assert.Equal(t, "My Home Drive", accounts[0].Mounts[0].DisplayName)
}

func TestBuildStatusAccountsWith_PausedOverridesNoToken(t *testing.T) {
	paused := true
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"): {
				SyncDir: "~/OneDrive",
				Paused:  &paused,
			},
		},
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockNameReader{},
		&mockSavedLoginChecker{reason: authReasonMissingLogin},
		&mockSyncStateQuerier{},
	)

	require.Len(t, accounts, 1)
	assert.Equal(t, driveStatePaused, accounts[0].Mounts[0].State)
	assert.Equal(t, authStateAuthenticationNeeded, accounts[0].AuthState)
}

func TestBuildStatusAccountsWith_EmptyConfig(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{},
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockNameReader{},
		&mockSavedLoginChecker{},
		&mockSyncStateQuerier{},
	)

	assert.Empty(t, accounts)
}

// --- 6.2b: Sync state and health summary tests ---

func TestBuildStatusAccountsWith_SyncStatePopulated(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"): {SyncDir: "~/OneDrive"},
		},
	}

	syncState := &syncStateInfo{
		FileCount:      45,
		ConditionCount: 2,
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockNameReader{},
		&mockSavedLoginChecker{},
		&mockSyncStateQuerier{state: syncState},
	)

	require.Len(t, accounts, 1)
	require.Len(t, accounts[0].Mounts, 1)
	require.NotNil(t, accounts[0].Mounts[0].SyncState)

	ss := accounts[0].Mounts[0].SyncState
	assert.Equal(t, 45, ss.FileCount)
	assert.Equal(t, 2, ss.ConditionCount)
}

func TestBuildStatusAccountsWith_NilSyncState(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"): {SyncDir: "~/OneDrive"},
		},
	}

	accounts := buildStatusAccountsWith(cfg,
		&mockNameReader{},
		&mockSavedLoginChecker{},
		&mockSyncStateQuerier{state: nil},
	)

	require.Len(t, accounts, 1)
	assert.Nil(t, accounts[0].Mounts[0].SyncState)
}

func TestQuerySyncState_NoDB(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)

	info := querySyncState("/nonexistent/path/state.db", logger)
	require.NotNil(t, info)
	assert.Zero(t, info.FileCount)
}

func TestQuerySyncState_EmptyDB(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	// Create a minimal DB with the required tables.
	createTestStateDB(t, dbPath)

	info := querySyncState(dbPath, logger)
	require.NotNil(t, info)
	assert.Equal(t, 0, info.FileCount)
	assert.Equal(t, 0, info.ConditionCount)
}

func TestQuerySyncState_WithBaselineEntries(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	createTestStateDB(t, dbPath)

	db, err := sql.Open("sqlite", "file:"+dbPath)
	require.NoError(t, err)
	defer db.Close()

	ctx := t.Context()

	// Insert a baseline entry.
	_, err = db.ExecContext(ctx, `INSERT INTO baseline (path, item_id, parent_id, item_type)
		VALUES ('/test.txt', 'item1', 'root', 'file')`)
	require.NoError(t, err)

	require.NoError(t, db.Close())

	info := querySyncState(dbPath, logger)
	require.NotNil(t, info)
	assert.Equal(t, 1, info.FileCount)
	assert.Zero(t, info.ConditionCount)
}

func TestQuerySyncState_RemoteDriftAndConditions(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	createTestStateDB(t, dbPath)

	db, err := sql.Open("sqlite", "file:"+dbPath)
	require.NoError(t, err)

	ctx := t.Context()
	testDriveID := driveid.New("test-drive-id")

	// Insert remote_state rows with mixed drift shapes.
	_, err = db.ExecContext(ctx, `INSERT INTO remote_state (path, item_id, item_type) VALUES
		('/a.txt', 'i1', 'file'),
		('/b.txt', 'i2', 'file'),
		('/c.txt', 'i3', 'file'),
		('/e.txt', 'i5', 'file')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO baseline (item_id, path, parent_id, item_type, remote_hash, remote_mtime) VALUES
		('i1', '/a.txt', 'root', 'file', '', 0),
		('i4', '/d.txt', 'root', 'file', '', 0)`)
	require.NoError(t, err)

	require.NoError(t, db.Close())

	store, err := syncengine.NewSyncStore(ctx, dbPath, logger)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, store.Close(ctx))
	}()

	_, err = store.RecordRetryWorkFailure(ctx, &syncengine.RetryWorkFailure{
		Work: syncengine.RetryWorkKey{
			Path:       "/x.txt",
			ActionType: syncengine.ActionUpload,
		},
	}, func(int) time.Duration { return 0 })
	require.NoError(t, err)
	_, err = store.RecordRetryWorkFailure(ctx, &syncengine.RetryWorkFailure{
		Work: syncengine.RetryWorkKey{
			Path:       "/x.txt",
			ActionType: syncengine.ActionUpload,
		},
	}, func(int) time.Duration { return 0 })
	require.NoError(t, err)
	_, err = store.RecordRetryWorkFailure(ctx, &syncengine.RetryWorkFailure{
		Work: syncengine.RetryWorkKey{
			Path:       "/x.txt",
			ActionType: syncengine.ActionUpload,
		},
	}, func(int) time.Duration { return 0 })
	require.NoError(t, err)
	_, err = store.RecordRetryWorkFailure(ctx, &syncengine.RetryWorkFailure{
		Work: syncengine.RetryWorkKey{
			Path:       "/y.txt",
			ActionType: syncengine.ActionUpload,
		},
	}, func(int) time.Duration { return 0 })
	require.NoError(t, err)
	_, err = store.RecordRetryWorkFailure(ctx, &syncengine.RetryWorkFailure{
		Work: syncengine.RetryWorkKey{
			Path:       "/y.txt",
			ActionType: syncengine.ActionUpload,
		},
	}, func(int) time.Duration { return 0 })
	require.NoError(t, err)
	_, err = store.RecordRetryWorkFailure(ctx, &syncengine.RetryWorkFailure{
		Work: syncengine.RetryWorkKey{
			Path:       "/y.txt",
			ActionType: syncengine.ActionUpload,
		},
	}, func(int) time.Duration { return 0 })
	require.NoError(t, err)
	_, err = store.RecordRetryWorkFailure(ctx, &syncengine.RetryWorkFailure{
		Work: syncengine.RetryWorkKey{
			Path:       "/y.txt",
			ActionType: syncengine.ActionUpload,
		},
	}, func(int) time.Duration { return 0 })
	require.NoError(t, err)
	seedObservationIssueForStatusTest(t, store, &syncengine.ObservationIssue{
		Path:      "/z.txt",
		DriveID:   testDriveID,
		IssueType: syncengine.IssueInvalidFilename,
	})

	info := querySyncState(dbPath, logger)
	require.NotNil(t, info)
	assert.Equal(t, 4, info.RemoteDrift)    // three remote-only creates plus one baseline row missing on remote
	assert.Equal(t, 1, info.ConditionCount) // 1 durable observation condition
	assert.Equal(t, 2, info.Retrying)       // 2 retry_work rows with attempt_count >= 3
}

// Validates: R-2.10.47, R-2.14.3
func TestQuerySyncState_CountsAuthAndRemoteBlockedScopesAsConditions(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	createTestStateDB(t, dbPath)

	db, err := sql.Open("sqlite", "file:"+dbPath)
	require.NoError(t, err)
	defer db.Close()

	ctx := t.Context()
	store, err := syncengine.NewSyncStore(ctx, dbPath, logger)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, store.Close(ctx))
	}()

	scopeKey := syncengine.SKPermRemoteWrite("Shared/Docs")
	require.NoError(t, store.UpsertBlockScope(ctx, &syncengine.BlockScope{
		Key:           scopeKey,
		TrialInterval: time.Minute,
		NextTrialAt:   time.Unix(0, 0).UTC().Add(time.Minute),
	}))
	_, err = store.RecordBlockedRetryWork(ctx, syncengine.RetryWorkKey{
		Path:       "/blocked/a.txt",
		ActionType: syncengine.ActionUpload,
	}, scopeKey)
	require.NoError(t, err)
	_, err = store.RecordBlockedRetryWork(ctx, syncengine.RetryWorkKey{
		Path:       "/blocked/b.txt",
		ActionType: syncengine.ActionUpload,
	}, scopeKey)
	require.NoError(t, err)
	seedObservationIssueForStatusTest(t, store, &syncengine.ObservationIssue{
		Path:      "/actionable.txt",
		DriveID:   driveid.New("test-drive-id"),
		IssueType: syncengine.IssueInvalidFilename,
	})

	info := querySyncState(dbPath, logger)
	require.NotNil(t, info)
	assert.Equal(t, 3, info.ConditionCount)
}

// Validates: R-6.10.5
func TestQuerySyncState_UsesReadOnlyStatusSnapshotHelper(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	store, err := syncengine.NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)

	require.NoError(t, store.CommitMutation(t.Context(), &syncengine.BaselineMutation{
		Action:   syncengine.ActionDownload,
		Success:  true,
		Path:     "/tracked.txt",
		DriveID:  driveid.New("test-drive-id"),
		ItemID:   "item-1",
		ParentID: "root",
		ItemType: syncengine.ItemTypeFile,
	}))

	walPath := dbPath + "-wal"
	shmPath := dbPath + "-shm"
	require.Eventually(t, func() bool {
		_, walErr := os.Stat(walPath)
		_, shmErr := os.Stat(shmPath)
		return walErr == nil && shmErr == nil
	}, time.Second, 10*time.Millisecond, "WAL sidecars were not created")

	require.NoError(t, os.Chmod(dbPath, 0o400))
	// #nosec G302 -- test intentionally makes the directory read-only to prove status stays on the read-only path.
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() {
		// #nosec G302 -- cleanup restores the tempdir so the writable store can close.
		assert.NoError(t, os.Chmod(dir, 0o700))
		assert.NoError(t, os.Chmod(dbPath, 0o600))
		assert.NoError(t, store.Close(context.Background()))
	})

	info := querySyncState(dbPath, logger)
	require.NotNil(t, info)
	assert.Zero(t, info.ConditionCount)
}

// Validates: R-2.10.4, R-2.10.32
func TestQuerySyncState_PreservesConditionScopeContext(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	createTestStateDB(t, dbPath)

	db, err := sql.Open("sqlite", "file:"+dbPath)
	require.NoError(t, err)
	defer db.Close()

	ctx := t.Context()
	store, err := syncengine.NewSyncStore(ctx, dbPath, logger)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, store.Close(ctx))
	}()

	scopeKey := syncengine.SKPermRemoteWrite("Shared/Team Docs")
	seedObservationIssueForStatusTest(t, store, &syncengine.ObservationIssue{
		Path:      "/invalid:name.txt",
		DriveID:   driveid.New("test-drive-id"),
		IssueType: syncengine.IssueInvalidFilename,
	})
	require.NoError(t, store.UpsertBlockScope(ctx, &syncengine.BlockScope{
		Key:           scopeKey,
		TrialInterval: time.Minute,
		NextTrialAt:   time.Unix(0, 0).UTC().Add(time.Minute),
	}))
	_, err = store.RecordBlockedRetryWork(ctx, syncengine.RetryWorkKey{
		Path:       "/blocked/a.txt",
		ActionType: syncengine.ActionUpload,
	}, scopeKey)
	require.NoError(t, err)
	_, err = store.RecordBlockedRetryWork(ctx, syncengine.RetryWorkKey{
		Path:       "/blocked/b.txt",
		ActionType: syncengine.ActionUpload,
	}, scopeKey)
	require.NoError(t, err)

	info := querySyncState(dbPath, logger)
	require.NotNil(t, info)
	invalidDescriptor := describeStatusCondition(syncengine.ConditionInvalidFilename)
	sharedDescriptor := describeStatusCondition(syncengine.ConditionRemoteWriteDenied)
	assert.ElementsMatch(t, []statusConditionJSON{
		{
			ConditionKey:  string(syncengine.ConditionInvalidFilename),
			ConditionType: string(syncengine.IssueInvalidFilename),
			Title:         invalidDescriptor.Title,
			Reason:        invalidDescriptor.Reason,
			Action:        invalidDescriptor.Action,
			Count:         1,
			Paths:         []string{"/invalid:name.txt"},
		},
		{
			ConditionKey:  string(syncengine.ConditionRemoteWriteDenied),
			ConditionType: string(syncengine.IssueRemoteWriteDenied),
			Title:         sharedDescriptor.Title,
			Reason:        sharedDescriptor.Reason,
			Action:        sharedDescriptor.Action,
			Count:         2,
			ScopeKind:     "directory",
			Scope:         "Shared/Team Docs",
			Paths:         []string{"/blocked/a.txt", "/blocked/b.txt"},
		},
	}, info.Conditions)
}

// Validates: R-2.10.32
func TestPrintStatusJSON_KeepsSameSummaryGroupsSeparatedByScope(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Email:     "alice@example.com",
			DriveType: "business",
			AuthState: authStateReady,
			Mounts: []statusMount{
				{
					CanonicalID: "business:alice@example.com",
					SyncDir:     "~/Work",
					State:       driveStateReady,
					SyncState: &syncStateInfo{
						FileCount:      10,
						ConditionCount: 2,
						Conditions: []statusConditionJSON{
							{
								ConditionKey: string(syncengine.ConditionQuotaExceeded),
								Title:        "QUOTA EXCEEDED",
								Count:        1,
								ScopeKind:    "drive",
								Scope:        "Shared/Docs",
							},
							{
								ConditionKey: string(syncengine.ConditionQuotaExceeded),
								Title:        "QUOTA EXCEEDED",
								Count:        1,
								ScopeKind:    "drive",
								Scope:        "Shared/Design",
							},
						},
					},
				},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printStatusJSON(&buf, accounts))

	var result statusOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	require.Len(t, result.Accounts, 1)
	require.Len(t, result.Accounts[0].Mounts, 1)
	require.Len(t, result.Accounts[0].Mounts[0].SyncState.Conditions, 2)
	assert.Equal(t, "Shared/Docs", result.Accounts[0].Mounts[0].SyncState.Conditions[0].Scope)
	assert.Equal(t, "Shared/Design", result.Accounts[0].Mounts[0].SyncState.Conditions[1].Scope)
}

// Validates: R-2.10.32
func TestPrintSyncStateText_KeepsSameSummaryGroupsSeparatedByScope(t *testing.T) {
	t.Parallel()

	quotaDescriptor := describeStatusCondition(syncengine.ConditionQuotaExceeded)
	ss := &syncStateInfo{
		ConditionCount: 2,
		Conditions: []statusConditionJSON{
			{
				ConditionKey:  string(syncengine.ConditionQuotaExceeded),
				ConditionType: string(syncengine.IssueQuotaExceeded),
				Title:         quotaDescriptor.Title,
				Reason:        quotaDescriptor.Reason,
				Action:        quotaDescriptor.Action,
				Count:         1,
				ScopeKind:     "drive",
				Scope:         "Shared/Docs",
			},
			{
				ConditionKey:  string(syncengine.ConditionQuotaExceeded),
				ConditionType: string(syncengine.IssueQuotaExceeded),
				Title:         quotaDescriptor.Title,
				Reason:        quotaDescriptor.Reason,
				Action:        quotaDescriptor.Action,
				Count:         1,
				ScopeKind:     "drive",
				Scope:         "Shared/Design",
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printSyncStateText(&buf, "    ", ss, false))
	output := buf.String()
	assert.Equal(t, 2, strings.Count(output, "QUOTA EXCEEDED (1 item)"))
	assert.Contains(t, output, "Scope: Shared/Docs")
	assert.Contains(t, output, "Scope: Shared/Design")
}

func TestComputeSummary_Mixed(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			AuthState: authStateReady,
			Mounts: []statusMount{
				{State: driveStateReady, SyncState: &syncStateInfo{ConditionCount: 3}},
				{State: driveStatePaused},
			},
		},
		{
			AuthState: authStateAuthenticationNeeded,
			Mounts: []statusMount{
				{State: driveStateReady},
				{State: driveStateReady, SyncState: &syncStateInfo{ConditionCount: 1}},
			},
		},
	}

	s := computeSummary(accounts)
	assert.Equal(t, 4, s.TotalMounts)
	assert.Equal(t, 3, s.Ready)
	assert.Equal(t, 1, s.Paused)
	assert.Equal(t, 1, s.AccountsRequiringAuth)
	assert.Equal(t, 4, s.TotalConditions)
}

func TestComputeSummary_IncludesNestedChildMounts(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			AuthState: authStateReady,
			Mounts: []statusMount{{
				State: driveStateReady,
				ChildMounts: []statusMount{
					{State: driveStatePaused, SyncState: &syncStateInfo{ConditionCount: 2}},
				},
			}},
		},
	}

	s := computeSummary(accounts)
	assert.Equal(t, 2, s.TotalMounts)
	assert.Equal(t, 1, s.Ready)
	assert.Equal(t, 1, s.Paused)
	assert.Equal(t, 2, s.TotalConditions)
}

func TestComputeSummary_Empty(t *testing.T) {
	t.Parallel()

	s := computeSummary(nil)
	assert.Equal(t, 0, s.TotalMounts)
	assert.Equal(t, 0, s.TotalConditions)
}

// --- printStatusJSON ---

func TestPrintStatusJSON_Empty(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := printStatusJSON(&buf, nil)
	require.NoError(t, err)

	var result statusOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	assert.Empty(t, result.Accounts)
}

func TestPrintStatusJSON_WithAccounts(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Email:     "alice@example.com",
			DriveType: "personal",
			AuthState: authStateReady,
			Mounts: []statusMount{
				{
					CanonicalID: "personal:alice@example.com",
					SyncDir:     "~/OneDrive",
					State:       driveStateReady,
					SyncState:   &syncStateInfo{FileCount: 10, ConditionCount: 1},
				},
			},
		},
	}

	var buf bytes.Buffer
	err := printStatusJSON(&buf, accounts)
	require.NoError(t, err)

	var result statusOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	require.Len(t, result.Accounts, 1)
	assert.Equal(t, "alice@example.com", result.Accounts[0].Email)
	assert.Equal(t, 1, result.Summary.Ready)
}

// Validates: R-2.10.4
func TestPrintStatusJSON_WithConditions(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Email:     "alice@example.com",
			DriveType: "personal",
			AuthState: authStateReady,
			Mounts: []statusMount{
				{
					CanonicalID: "personal:alice@example.com",
					SyncDir:     "~/OneDrive",
					State:       driveStateReady,
					SyncState: &syncStateInfo{
						FileCount:      10,
						ConditionCount: 3,
						Conditions: []statusConditionJSON{
							{
								ConditionKey: string(syncengine.ConditionQuotaExceeded),
								Title:        "QUOTA EXCEEDED",
								Count:        1,
								ScopeKind:    "drive",
								Scope:        "Shared/Team Docs",
							},
							{
								ConditionKey: string(syncengine.ConditionInvalidFilename),
								Title:        "INVALID FILENAME",
								Count:        2,
								ScopeKind:    "file",
							},
						},
					},
				},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printStatusJSON(&buf, accounts))

	var result statusOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	require.Len(t, result.Accounts, 1)
	require.Len(t, result.Accounts[0].Mounts, 1)
	require.NotNil(t, result.Accounts[0].Mounts[0].SyncState)
	require.Len(t, result.Accounts[0].Mounts[0].SyncState.Conditions, 2)
	assert.Equal(t, "drive", result.Accounts[0].Mounts[0].SyncState.Conditions[0].ScopeKind)
	assert.Equal(t, "Shared/Team Docs", result.Accounts[0].Mounts[0].SyncState.Conditions[0].Scope)
}

func TestPrintStatusJSON_SyncStateOmitsLegacyHistoryKeys(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Email:     "alice@example.com",
			DriveType: "personal",
			AuthState: authStateReady,
			Mounts: []statusMount{
				{
					CanonicalID: "personal:alice@example.com",
					SyncDir:     "~/OneDrive",
					State:       driveStateReady,
					SyncState: &syncStateInfo{
						FileCount:      12,
						ConditionCount: 0,
						ExamplesLimit:  5,
					},
				},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printStatusJSON(&buf, accounts))

	var decoded struct {
		Accounts []struct {
			Mounts []struct {
				SyncState json.RawMessage `json:"sync_state"`
			} `json:"mounts"`
		} `json:"accounts"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	require.Len(t, decoded.Accounts, 1)
	require.Len(t, decoded.Accounts[0].Mounts, 1)
	require.NotEmpty(t, decoded.Accounts[0].Mounts[0].SyncState)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(decoded.Accounts[0].Mounts[0].SyncState, &raw))
	assert.Contains(t, raw, "file_count")
	assert.NotContains(t, raw, "last_sync_time")
	assert.NotContains(t, raw, "last_sync_duration")
	assert.NotContains(t, raw, "last_error")
}

// --- printStatusText ---

func TestPrintStatusText_NoMounts(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printStatusText(&buf, nil, false))

	output := buf.String()
	assert.Equal(t, "Summary: 0 mounts, 0 conditions\n", output)
}

func TestPrintStatusText_WithDisplayNameAndOrg(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Email:       "alice@contoso.com",
			DisplayName: "Alice Smith",
			DriveType:   "business",
			OrgName:     "Contoso",
			AuthState:   authStateReady,
			Mounts: []statusMount{
				{
					CanonicalID: "business:alice@contoso.com",
					SyncDir:     "~/Work",
					State:       driveStateReady,
				},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printStatusText(&buf, accounts, false))

	output := buf.String()
	assert.True(t, strings.HasPrefix(output, "Summary: 1 mounts (1 ready), 0 conditions\n\n"))
	assert.Contains(t, output, "Alice Smith (alice@contoso.com)")
	assert.Contains(t, output, "Org:   Contoso")
	assert.Contains(t, output, "Auth:  ready")
	assert.Contains(t, output, "~/Work")
}

func TestPrintStatusText_WithAuthRequiredReasonAndAction(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Email:      "alice@example.com",
			DriveType:  "personal",
			AuthState:  authStateAuthenticationNeeded,
			AuthReason: string(authReasonSyncAuthRejected),
			AuthAction: authAction(authReasonSyncAuthRejected),
			Mounts: []statusMount{
				{
					CanonicalID: "personal:alice@example.com",
					SyncDir:     "~/OneDrive",
					State:       driveStateReady,
				},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printStatusText(&buf, accounts, false))

	output := buf.String()
	assert.Contains(t, output, "Auth:  authentication_required")
	assert.Contains(t, output, "The last sync attempt for this account was rejected by OneDrive.")
	assert.Contains(t, output, "status")
}

func TestPrintStatusText_SyncStateNever(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Email:     "bob@example.com",
			DriveType: "personal",
			AuthState: authStateReady,
			Mounts: []statusMount{
				{
					CanonicalID: "personal:bob@example.com",
					SyncDir:     "~/OneDrive",
					State:       driveStateReady,
					SyncState:   &syncStateInfo{},
				},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printStatusText(&buf, accounts, false))

	output := buf.String()
	assert.Contains(t, output, "No active conditions.")
}

func TestPrintStatusText_EmptySyncDir(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Email:     "bob@example.com",
			DriveType: "personal",
			AuthState: authStateReady,
			Mounts: []statusMount{
				{
					CanonicalID: "personal:bob@example.com",
					SyncDir:     "",
					State:       driveStateReady,
				},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printStatusText(&buf, accounts, false))

	output := buf.String()
	assert.Contains(t, output, syncDirNotSet)
}

func TestPrintSummaryText_AllStates(t *testing.T) {
	t.Parallel()

	s := statusSummary{
		TotalMounts:           4,
		Ready:                 3,
		Paused:                1,
		AccountsRequiringAuth: 1,
		TotalConditions:       3,
	}

	var buf bytes.Buffer
	require.NoError(t, printSummaryText(&buf, s))

	output := buf.String()
	assert.Contains(t, output, "4 mounts")
	assert.Contains(t, output, "3 ready")
	assert.Contains(t, output, "1 paused")
	assert.Contains(t, output, "1 accounts requiring auth")
	assert.Contains(t, output, "3 conditions")
}

func TestPrintSummaryText_WithPendingAndRetrying(t *testing.T) {
	t.Parallel()

	s := statusSummary{
		TotalMounts:      2,
		Ready:            2,
		TotalConditions:  1,
		TotalRemoteDrift: 5,
		TotalRetrying:    3,
	}

	var buf bytes.Buffer
	require.NoError(t, printSummaryText(&buf, s))

	output := buf.String()
	assert.Contains(t, output, "5 remote drift")
	assert.Contains(t, output, "3 retrying")
}

// Validates: R-2.10.4
func TestPrintSyncStateText_WithPendingAndConditions(t *testing.T) {
	t.Parallel()

	ss := &syncStateInfo{
		FileCount:      45,
		ConditionCount: 0,
		RemoteDrift:    3,
		Retrying:       2,
	}

	var buf bytes.Buffer
	require.NoError(t, printSyncStateText(&buf, "    ", ss, false))

	output := buf.String()
	assert.Contains(t, output, "Remote drift: 3 items")
	assert.Contains(t, output, "Retrying:  2 items")
}

// Validates: R-2.10.4
func TestPrintSyncStateText_WithConditions(t *testing.T) {
	t.Parallel()

	quotaDescriptor := describeStatusCondition(syncengine.ConditionQuotaExceeded)
	invalidDescriptor := describeStatusCondition(syncengine.ConditionInvalidFilename)
	authDescriptor := describeStatusCondition(syncengine.ConditionAuthenticationRequired)
	ss := &syncStateInfo{
		ConditionCount: 3,
		Retrying:       2,
		Conditions: []statusConditionJSON{
			{
				ConditionKey:  string(syncengine.ConditionQuotaExceeded),
				ConditionType: string(syncengine.IssueQuotaExceeded),
				Title:         quotaDescriptor.Title,
				Reason:        quotaDescriptor.Reason,
				Action:        quotaDescriptor.Action,
				Count:         1,
				ScopeKind:     "drive",
				Scope:         "Shared/Team Docs",
				Paths:         []string{"/quota/a.txt"},
			},
			{
				ConditionKey:  string(syncengine.ConditionInvalidFilename),
				ConditionType: string(syncengine.IssueInvalidFilename),
				Title:         invalidDescriptor.Title,
				Reason:        invalidDescriptor.Reason,
				Action:        invalidDescriptor.Action,
				Count:         2,
				Paths:         []string{"/bad:name.txt", "/worse:name.txt"},
			},
			{
				ConditionKey:  string(syncengine.ConditionAuthenticationRequired),
				ConditionType: string(syncengine.IssueUnauthorized),
				Title:         authDescriptor.Title,
				Reason:        authDescriptor.Reason,
				Action:        authDescriptor.Action,
				Count:         1,
				ScopeKind:     "account",
				Scope:         "your OneDrive account authorization",
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printSyncStateText(&buf, "    ", ss, false))
	requireGoldenText(t, "status_sync_state_with_conditions.golden", buf.String())
}

func requireSingleStatusDriveJSON(
	t *testing.T,
	decoded statusOutput,
	canonicalID string,
) (statusMount, *syncStateInfo) {
	t.Helper()

	drive, syncState := findStatusDriveJSON(t, decoded, canonicalID)
	require.Equal(t, 1, decoded.Summary.TotalMounts, "expected filtered status output")
	return drive, syncState
}

func findStatusDriveJSON(
	t *testing.T,
	decoded statusOutput,
	canonicalID string,
) (statusMount, *syncStateInfo) {
	t.Helper()

	var (
		foundDrive statusMount
		found      bool
	)
	for i := range decoded.Accounts {
		for j := range decoded.Accounts[i].Mounts {
			drive, ok := findStatusMountRecursive(&decoded.Accounts[i].Mounts[j], canonicalID)
			if ok {
				require.False(t, found, "expected exactly one drive in filtered status output")
				foundDrive = drive
				found = true
			}
		}
	}
	require.True(t, found, "expected drive %s in status output", canonicalID)
	return foundDrive, foundDrive.SyncState
}

func findStatusMountRecursive(mount *statusMount, identity string) (statusMount, bool) {
	if mount == nil {
		return statusMount{}, false
	}
	if mount.CanonicalID == identity || mount.MountID == identity {
		return *mount, true
	}
	for i := range mount.ChildMounts {
		if found, ok := findStatusMountRecursive(&mount.ChildMounts[i], identity); ok {
			return found, true
		}
	}

	return statusMount{}, false
}

func TestStatusCommand_UnreadableStateStoreFallsBackToEmptySyncState(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:damaged@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive"))
	seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
		account.DisplayName = "Damaged User"
	})
	seedCatalogDrive(t, cid, func(drive *config.CatalogDrive) {
		drive.RemoteDriveID = "drive-damaged"
	})
	require.NoError(t, os.MkdirAll(filepath.Dir(config.DriveStatePath(cid)), 0o700))
	require.NoError(t, os.WriteFile(config.DriveStatePath(cid), []byte("not a sqlite database"), 0o600))

	var out bytes.Buffer
	cc := newCommandContext(&out, cfgPath)
	cc.Flags.Drive = []string{cid.String()}
	cc.Flags.JSON = true

	require.NoError(t, runStatusCommand(cc, false))

	var decoded statusOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	_, syncState := requireSingleStatusDriveJSON(t, decoded, cid.String())
	require.NotNil(t, syncState)
	assert.Zero(t, syncState.FileCount)
}

func TestComputeSummary_AggregatesPendingAndRetrying(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Mounts: []statusMount{
				{State: driveStateReady, SyncState: &syncStateInfo{RemoteDrift: 3, Retrying: 1}},
				{State: driveStateReady, SyncState: &syncStateInfo{RemoteDrift: 2, Retrying: 4}},
			},
		},
	}

	s := computeSummary(accounts)
	assert.Equal(t, 5, s.TotalRemoteDrift)
	assert.Equal(t, 5, s.TotalRetrying)
}

// createTestStateDB creates a minimal SQLite DB with tables matching the sync schema.
func createTestStateDB(t *testing.T, dbPath string) {
	t.Helper()

	store, err := syncengine.NewSyncStore(t.Context(), dbPath, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	require.NoError(t, store.Close(t.Context()))
}
