package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
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

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

// statusAccountView is what buildAccountViews makes of an account that has
// one configured drive, its own, and a usable saved login: the input status
// projects into its account row.
func statusAccountView(displayName string, cid driveid.CanonicalID) accountView {
	return accountView{
		Email:              cid.Email(),
		DriveType:          cid.DriveType(),
		DisplayName:        displayName,
		Configured:         true,
		ConfiguredDriveIDs: []driveid.CanonicalID{cid},
		AuthHealth:         evaluateAccountViewAuth("", ""),
	}
}

// statusAccountsThroughViews builds status accounts the way status does:
// account views from the config and the catalog on disk, then the status
// projection of each view, with no sync state behind the drives.
func statusAccountsThroughViews(t *testing.T, cfg *config.Config) []statusAccount {
	t.Helper()

	stored, err := config.LoadCatalog()
	require.NoError(t, err)
	views, err := buildAccountViews(t.Context(), cfg, stored, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	return buildStatusAccountsFromViews(t.Context(), cfg, nil, views, &mockSyncStateQuerier{})
}

// seedConfiguredStatusDrives records every configured drive and the account
// that owns it in the catalog, as login and drive add do. A SharePoint
// library's owner is the business account of its email.
func seedConfiguredStatusDrives(t *testing.T, cfg *config.Config) {
	t.Helper()

	for cid := range cfg.Drives {
		owner := cid
		if cid.IsSharePoint() {
			owner = driveid.MustCanonicalID(driveid.DriveTypeBusiness + ":" + cid.Email())
		}
		seedCatalogAccount(t, owner, nil)
		seedCatalogDrive(t, cid, nil)
	}
}

// queryLiveSyncState reads a state database through the querier status uses
// for a configured drive, with no content filter and default verbosity.
func queryLiveSyncState(ctx context.Context, statePath string, logger *slog.Logger) *syncStateInfo {
	querier := &liveSyncStateQuerier{logger: logger, examplesLimit: defaultVisiblePaths}

	return querier.QuerySyncState(ctx, statePath, statusStoreRootDrive, syncdomain.ContentFilterConfig{})
}

// mockSyncStateQuerier returns a fixed sync state for all drives.
type mockSyncStateQuerier struct {
	state *syncStateInfo
}

func (m *mockSyncStateQuerier) QuerySyncState(
	_ context.Context, _ string, _ statusStoreRoot, _ syncdomain.ContentFilterConfig,
) *syncStateInfo {
	return m.state
}

// seedObservationIssueForStatusTest stores one issue. A batch replaces every
// stored issue of the types it manages, so call it once per type on a store
// that holds none of that type yet, as every caller does.
func seedObservationIssueForStatusTest(
	t *testing.T,
	store *syncstore.SyncStore,
	issue *syncdomain.ObservationIssue,
) {
	t.Helper()

	require.NoError(t, store.ReconcileObservationFindings(
		t.Context(),
		&syncdomain.ObservationFindingsBatch{
			Issues:            []syncdomain.ObservationIssue{*issue},
			ManagedIssueTypes: []syncdomain.IssueType{issue.IssueType},
		},
	))
}

func testShortcutStatusChild(
	parentCID driveid.CanonicalID,
	state syncshortcut.ShortcutRootState,
) childMountStatusInput {
	record := testShortcutStatusRecord(parentCID, state)
	return testShortcutStatusChildFromRecord(parentCID, &record)
}

func testShortcutStatusRecord(
	parentCID driveid.CanonicalID,
	state syncshortcut.ShortcutRootState,
) syncshortcut.ShortcutRootRecord {
	const (
		bindingID    = "binding-docs"
		relativePath = "Shortcuts/Docs"
	)
	alias := filepath.Base(relativePath)
	return syncshortcut.ShortcutRootRecord{
		NamespaceID:       parentCID.String(),
		BindingItemID:     bindingID,
		RelativeLocalPath: relativePath,
		LocalAlias:        alias,
		RemoteDriveID:     driveid.New("remote-drive"),
		RemoteItemID:      "remote-root",
		RemoteIsFolder:    true,
		State:             state,
		ProtectedPaths:    []string{relativePath},
	}
}

func testShortcutStatusChildFromRecord(
	parentCID driveid.CanonicalID,
	record *syncshortcut.ShortcutRootRecord,
) childMountStatusInput {
	metadata := syncshortcut.ShortcutRootStatus(record.State)
	parentSyncRoot := filepath.Clean("/tmp/sync-root")
	statusView := syncshortcut.ShortcutRootStatusView{
		MountID:          config.ChildMountID(parentCID.String(), record.BindingItemID),
		SortPath:         record.RelativeLocalPath,
		DisplayName:      record.LocalAlias,
		DisplayLocalRoot: filepath.Join(parentSyncRoot, filepath.FromSlash(record.RelativeLocalPath)),
		Metadata:         metadata,
		Reason:           metadata.Issue,
		Detail:           record.BlockedDetail,
	}
	if record.Waiting != nil {
		statusView.WaitingReplacementPath = record.Waiting.RelativeLocalPath
	}
	if metadata.ProtectsPath {
		statusView.ProtectedCurrentLocalRoot = filepath.Join(parentSyncRoot, filepath.FromSlash(record.RelativeLocalPath))
		for _, protectedPath := range record.ProtectedPaths {
			if protectedPath == "" || protectedPath == record.RelativeLocalPath {
				continue
			}
			statusView.ProtectedReservedLocalRoots = append(
				statusView.ProtectedReservedLocalRoots,
				filepath.Join(parentSyncRoot, filepath.FromSlash(protectedPath)),
			)
		}
	}
	return childMountStatusInput{
		ParentID: parentCID,
		Root:     statusView,
	}
}

func TestDriveState_Ready(t *testing.T) {
	d := &config.Drive{}
	assert.Equal(t, driveStateReady, driveState(d))
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
func TestBuildChildStatusDrive_InheritsParentPause(t *testing.T) {
	parentCID := driveid.MustCanonicalID("personal:alice@example.com")
	child := testShortcutStatusChild(parentCID, syncshortcut.ShortcutRootStateActive)
	paused := true
	mount := buildChildStatusDrive(
		t.Context(),
		&config.Drive{SyncDir: "/tmp/sync-root", Paused: &paused},
		&child,
		nil,
	)

	assert.Equal(t, driveStatePaused, mount.State)
}

type shortcutStatusLifecycleCase struct {
	name               string
	state              syncshortcut.ShortcutRootState
	displayState       string
	reason             string
	detail             string
	action             string
	autoRetry          *bool
	protectsPath       bool
	waitingReplacement string
}

func shortcutStatusLifecycleCases() []shortcutStatusLifecycleCase {
	cases := []shortcutStatusLifecycleCase{
		{
			name:         "active",
			state:        syncshortcut.ShortcutRootStateActive,
			displayState: driveStateReady,
		},
		{
			name:         "target_unavailable",
			state:        syncshortcut.ShortcutRootStateTargetUnavailable,
			displayState: string(syncshortcut.ShortcutRootStateTargetUnavailable),
			reason:       string(syncshortcut.ShortcutRootStateTargetUnavailable),
			detail:       "OneDrive can't open this shared folder; it may have been unshared, moved or deleted.",
			action:       "Ask the owner to share it with you again, or remove the shortcut from your OneDrive.",
			autoRetry:    boolPtr(true),
			protectsPath: true,
		},
		{
			name:         "local_root_unavailable",
			state:        syncshortcut.ShortcutRootStateLocalRootUnavailable,
			displayState: string(syncshortcut.ShortcutRootStateLocalRootUnavailable),
			reason:       string(syncshortcut.ShortcutRootStateLocalRootUnavailable),
			detail:       "This shared folder's local folder is missing or can't be read.",
			action:       "Restore the local folder, or delete it to discard changes that have not synced.",
			autoRetry:    boolPtr(true),
			protectsPath: true,
		},
		{
			name:         "blocked_path",
			state:        syncshortcut.ShortcutRootStateBlockedPath,
			displayState: string(syncshortcut.ShortcutRootStateBlockedPath),
			reason:       string(syncshortcut.ShortcutRootStateBlockedPath),
			detail:       "Something else is in the way at this shared folder's local path.",
			action:       "Move or rename the file or folder at that path.",
			autoRetry:    boolPtr(true),
			protectsPath: true,
		},
		{
			name:         "rename_ambiguous",
			state:        syncshortcut.ShortcutRootStateRenameAmbiguous,
			displayState: string(syncshortcut.ShortcutRootStateRenameAmbiguous),
			reason:       string(syncshortcut.ShortcutRootStateRenameAmbiguous),
			detail:       "More than one renamed folder could be this shared folder.",
			action:       "Keep exactly one renamed folder, or restore the original name.",
			autoRetry:    boolPtr(true),
			protectsPath: true,
		},
		{
			name:         "alias_mutation_blocked",
			state:        syncshortcut.ShortcutRootStateAliasMutationBlocked,
			displayState: string(syncshortcut.ShortcutRootStateAliasMutationBlocked),
			reason:       string(syncshortcut.ShortcutRootStateAliasMutationBlocked),
			detail:       "Renaming or deleting this shared folder locally couldn't be applied in OneDrive.",
			action:       "Check your connection and your access to the folder, or undo the local rename or delete.",
			autoRetry:    boolPtr(true),
			protectsPath: true,
		},
	}
	return append(cases, shortcutStatusRemovalLifecycleCases()...)
}

func shortcutStatusRemovalLifecycleCases() []shortcutStatusLifecycleCase {
	return []shortcutStatusLifecycleCase{
		{
			name:         "removed_final_drain",
			state:        syncshortcut.ShortcutRootStateRemovedFinalDrain,
			displayState: string(syncshortcut.ShortcutRootStateRemovedFinalDrain),
			reason:       string(syncshortcut.ShortcutRootStateRemovedFinalDrain),
			detail:       "This shared folder was removed from your OneDrive; its last local changes are syncing before the local copy is removed.",
			action:       "Restore access to the shared folder, or delete its local folder to discard changes that have not synced.",
			autoRetry:    boolPtr(true),
			protectsPath: true,
		},
		{
			name:         "removed_cleanup_blocked",
			state:        syncshortcut.ShortcutRootStateRemovedCleanupBlocked,
			displayState: string(syncshortcut.ShortcutRootStateRemovedCleanupBlocked),
			reason:       string(syncshortcut.ShortcutRootStateRemovedCleanupBlocked),
			detail:       "This shared folder was removed, but its local copy can't be removed.",
			action:       "Move or delete what is in the way at that path; the next sync finishes the cleanup.",
			autoRetry:    boolPtr(true),
			protectsPath: true,
		},
		{
			name:         "removed_release_pending",
			state:        syncshortcut.ShortcutRootStateRemovedReleasePending,
			displayState: string(syncshortcut.ShortcutRootStateRemovedReleasePending),
			reason:       string(syncshortcut.ShortcutRootStateRemovedReleasePending),
			detail:       "This shared folder was removed and its last changes have synced; its local copy is being removed.",
			autoRetry:    boolPtr(true),
			protectsPath: true,
		},
		{
			name:         "removed_child_cleanup_pending",
			state:        syncshortcut.ShortcutRootStateRemovedChildCleanupPending,
			displayState: string(syncshortcut.ShortcutRootStateRemovedChildCleanupPending),
			reason:       string(syncshortcut.ShortcutRootStateRemovedChildCleanupPending),
			detail:       "This shared folder was removed; its local sync data is being cleaned up.",
			autoRetry:    boolPtr(true),
		},
		{
			name:               "same_path_replacement_waiting",
			state:              syncshortcut.ShortcutRootStateSamePathReplacementWaiting,
			displayState:       string(syncshortcut.ShortcutRootStateSamePathReplacementWaiting),
			reason:             string(syncshortcut.ShortcutRootStateSamePathReplacementWaiting),
			detail:             "A new shared folder with the same name is waiting for the old one to finish syncing.",
			autoRetry:          boolPtr(true),
			protectsPath:       true,
			waitingReplacement: "Shortcuts/Docs",
		},
		{
			name:         "duplicate_target",
			state:        syncshortcut.ShortcutRootStateDuplicateTarget,
			displayState: string(syncshortcut.ShortcutRootStateDuplicateTarget),
			reason:       string(syncshortcut.ShortcutRootStateDuplicateTarget),
			detail:       "Another shortcut in this drive already syncs the same shared folder.",
			action:       "Remove one of the two shortcuts from your OneDrive.",
			autoRetry:    boolPtr(true),
			protectsPath: true,
		},
	}
}

// Validates: R-2.10.1
func TestBuildChildStatusDrive_RendersLifecycleState(t *testing.T) {
	parentCID := driveid.MustCanonicalID("personal:alice@example.com")
	tests := shortcutStatusLifecycleCases()
	for i := range tests {
		t.Run(tests[i].name, func(t *testing.T) {
			tc := tests[i]
			mount := buildStatusLifecycleMount(t, parentCID, &tc)
			assertStatusLifecycleMount(t, &mount, &tc)
			assertStatusLifecycleText(t, &mount, &tc)
			assertStatusLifecycleJSON(t, &mount, &tc)
		})
	}
}

func buildStatusLifecycleMount(
	t *testing.T,
	parentCID driveid.CanonicalID,
	tc *shortcutStatusLifecycleCase,
) statusDrive {
	t.Helper()

	record := testShortcutStatusRecord(parentCID, tc.state)
	if tc.waitingReplacement != "" {
		record.Waiting = &syncshortcut.ShortcutRootReplacement{
			BindingItemID:     "binding-new",
			RelativeLocalPath: tc.waitingReplacement,
			LocalAlias:        "Docs",
			RemoteDriveID:     driveid.New("remote-drive-new"),
			RemoteItemID:      "remote-root-new",
			RemoteIsFolder:    true,
		}
	}
	child := testShortcutStatusChildFromRecord(parentCID, &record)
	return buildChildStatusDrive(t.Context(), &config.Drive{SyncDir: "/tmp/sync-root"}, &child, nil)
}

func assertStatusLifecycleMount(t *testing.T, mount *statusDrive, tc *shortcutStatusLifecycleCase) {
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
	if tc.protectsPath {
		assert.Equal(t, "/tmp/sync-root/Shortcuts/Docs", mount.ProtectedCurrentPath)
	} else {
		assert.Empty(t, mount.ProtectedCurrentPath)
		assert.Empty(t, mount.ProtectedReservedPaths)
	}
}

func assertStatusLifecycleText(t *testing.T, mount *statusDrive, tc *shortcutStatusLifecycleCase) {
	t.Helper()

	var text bytes.Buffer
	require.NoError(t, printDriveStatus(&text, mount, 0))
	rendered := text.String()
	assert.NotContains(t, rendered, "onedrive-go ")
	assert.NotContains(t, rendered, "Run ")
	if tc.reason != "" {
		assert.Contains(t, rendered, "Reason: "+tc.detail)
		assert.NotContains(t, rendered, tc.reason, "the machine state token is for --json only")
		assert.Contains(t, rendered, "Auto retry: yes")
		if tc.protectsPath {
			assert.Contains(t, rendered, "Protected current path: /tmp/sync-root/Shortcuts/Docs")
		} else {
			assert.NotContains(t, rendered, "Protected current path:")
		}
	}
	if tc.action != "" {
		assert.Contains(t, rendered, "Action: "+tc.action)
	}
	if tc.waitingReplacement != "" {
		assert.Contains(t, rendered, "Waiting replacement: "+tc.waitingReplacement)
	}
}

func assertStatusLifecycleJSON(t *testing.T, mount *statusDrive, tc *shortcutStatusLifecycleCase) {
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
	assert.Contains(t, jsonText, `"auto_retry":true`)
	if tc.protectsPath {
		assert.Contains(t, jsonText, `"protected_current_path":`)
	} else {
		assert.NotContains(t, jsonText, `"protected_current_path":`)
	}
}

func boolPtr(value bool) *bool {
	return &value
}

// Validates: R-2.3.3, R-2.4.8, R-2.10.4
func TestBuildChildStatusDrive_SurfacesProtectedPaths(t *testing.T) {
	parentCID := driveid.MustCanonicalID("personal:alice@example.com")
	record := testShortcutStatusRecord(parentCID, syncshortcut.ShortcutRootStateRemovedFinalDrain)
	record.ProtectedPaths = []string{"Shortcuts/Docs", "Shortcuts/Old Docs"}
	child := testShortcutStatusChildFromRecord(parentCID, &record)
	mount := buildChildStatusDrive(
		t.Context(),
		&config.Drive{SyncDir: "/tmp/sync-root"},
		&child,
		nil,
	)

	assert.Equal(t, "/tmp/sync-root/Shortcuts/Docs", mount.ProtectedCurrentPath)
	assert.Equal(t, []string{"/tmp/sync-root/Shortcuts/Old Docs"}, mount.ProtectedReservedPaths)
	assert.Contains(t, mount.StateDetail, "last local changes are syncing")
	assert.NotContains(t, mount.RecoveryAction, "rerun sync")
}

// Validates: R-2.10.1
func TestBuildStatusAccountsFromViews_NestsChildMountsUnderParent(t *testing.T) {
	setTestDriveHome(t)

	parentCID := driveid.MustCanonicalID("personal:alice@example.com")
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			parentCID: {SyncDir: "/tmp/sync-root"},
		},
	}
	child := testShortcutStatusChild(parentCID, syncshortcut.ShortcutRootStateActive)
	shortcutRoots := map[driveid.CanonicalID][]syncshortcut.ShortcutRootStatusView{
		parentCID: {child.Root},
	}

	accounts := buildStatusAccountsFromViews(t.Context(), cfg, shortcutRoots,
		[]accountView{statusAccountView("", parentCID)}, nil)

	require.Len(t, accounts, 1)
	account := accounts[0]
	require.Len(t, account.Drives, 1)
	parent := account.Drives[0]
	assert.Equal(t, parentCID.String(), parent.InternalID)
	assert.Empty(t, parent.NamespaceID)
	require.Len(t, parent.SharedFolders, 1)
	assert.Equal(t, config.ChildMountID(parentCID.String(), "binding-docs"), parent.SharedFolders[0].InternalID)
	assert.Equal(t, parentCID.String(), parent.SharedFolders[0].NamespaceID)
}

// Validates: R-2.10.1
func TestPrintMountStatus_RendersChildLifecycleReasonAndNextAction(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := printDriveStatus(&buf, asBuiltStatusDrive(&statusDrive{
		InternalID:  "child-docs",
		NamespaceID: "personal:alice@example.com",
		Kind:        statusDriveKindSharedFolder,
		Name:        "Docs",
		Folder:      "/tmp/sync-root/Shortcuts/Docs",
		State:       string(syncshortcut.ShortcutRootStateTargetUnavailable),
		StateLabel:  "Shared folder unavailable",
		StateReason: string(syncshortcut.ShortcutRootStateTargetUnavailable),
		StateDetail: "OneDrive can't open this shared folder; it may have been unshared, moved or deleted.",
	}), 0)
	require.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, "Status: Shared folder unavailable")
	assert.Contains(t, output, "Reason: OneDrive can't open this shared folder")
	assert.NotContains(t, output, string(syncshortcut.ShortcutRootStateTargetUnavailable))
	assert.Contains(t, output, "Shared folder: Managed through the parent drive")
}

// Validates: R-2.3.3, R-2.4.8, R-2.10.4
func TestPrintMountStatus_RendersGuidedShortcutRecovery(t *testing.T) {
	t.Parallel()

	autoRetry := true
	var buf bytes.Buffer
	err := printDriveStatus(&buf, asBuiltStatusDrive(&statusDrive{
		InternalID:             "child-docs",
		NamespaceID:            "personal:alice@example.com",
		Kind:                   statusDriveKindSharedFolder,
		Name:                   "Docs",
		Folder:                 "/tmp/sync-root/Shortcuts/Docs",
		State:                  string(syncshortcut.ShortcutRootStateRemovedFinalDrain),
		StateReason:            string(syncshortcut.ShortcutRootStateRemovedFinalDrain),
		StateDetail:            "This shared folder was removed from your OneDrive; its last local changes are syncing before the local copy is removed.",
		ProtectedCurrentPath:   "/tmp/sync-root/Shortcuts/Docs",
		ProtectedReservedPaths: []string{"/tmp/sync-root/Shortcuts/Old Docs"},
		AutoRetry:              &autoRetry,
	}), 0)
	require.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, "Protected current path: /tmp/sync-root/Shortcuts/Docs")
	assert.Contains(t, output, "Reserved path: /tmp/sync-root/Shortcuts/Old Docs")
	assert.Contains(t, output, "Reason: This shared folder was removed")
	assert.Contains(t, output, "Auto retry: yes")
}

// Validates: R-2.10.1
func TestBuildChildStatusDrive_UsesMountIDWithoutSyntheticSharedCanonical(t *testing.T) {
	parentCID := driveid.MustCanonicalID("personal:alice@example.com")
	record := testShortcutStatusRecord(parentCID, syncshortcut.ShortcutRootStateBlockedPath)
	record.BlockedDetail = "shortcut alias path is blocked by a local file"
	child := testShortcutStatusChildFromRecord(parentCID, &record)
	mount := buildChildStatusDrive(
		t.Context(),
		&config.Drive{SyncDir: "/tmp/sync-root"},
		&child,
		nil,
	)

	assert.Equal(t, config.ChildMountID(parentCID.String(), "binding-docs"), mount.InternalID)
	assert.Equal(t, parentCID.String(), mount.NamespaceID)
	assert.Equal(t, statusDriveKindSharedFolder, mount.Kind)
	assert.Equal(t, "Docs", mount.Name)
	assert.Equal(t, string(syncshortcut.ShortcutRootStateBlockedPath), mount.StateReason)
	assert.Equal(t, "Something else is in the way at this shared folder's local path.", mount.StateDetail)
	assert.Equal(t, "shortcut alias path is blocked by a local file", mount.StateDebugDetail)
	assert.Contains(t, mount.RecoveryAction, "Move or rename the file or folder at that path")
	require.NotNil(t, mount.AutoRetry)
	assert.True(t, *mount.AutoRetry)
	assert.NotContains(t, mount.StateDetail, "pause")

	encoded, err := json.Marshal(mount)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "mount_id")
	assert.NotContains(t, string(encoded), "namespace_id")
	assert.Contains(t, string(encoded), `"state_reason":"blocked_path"`)
	assert.Contains(t, string(encoded), `"state_detail":`)
	assert.Contains(t, string(encoded), `"recovery_action":`)
	assert.Contains(t, string(encoded), `"auto_retry":true`)
	assert.NotContains(t, string(encoded), "canonical_id")
	assert.NotContains(t, string(encoded), "shared:")
}

// Validates: R-2.10.1, R-2.4.8
func TestBuildChildStatusDrive_FinalDrainGuidesAccessRestore(t *testing.T) {
	t.Parallel()

	parentCID := driveid.MustCanonicalID("personal:alice@example.com")
	child := testShortcutStatusChild(parentCID, syncshortcut.ShortcutRootStateRemovedFinalDrain)

	mount := buildChildStatusDrive(
		t.Context(),
		&config.Drive{SyncDir: "/tmp/sync-root"},
		&child,
		nil,
	)

	assert.Equal(t, string(syncshortcut.ShortcutRootStateRemovedFinalDrain), mount.State)
	assert.Equal(t, string(syncshortcut.ShortcutRootStateRemovedFinalDrain), mount.StateReason)
	assert.Contains(t, mount.StateDetail, "last local changes are syncing")
	assert.Contains(t, mount.RecoveryAction, "Restore access to the shared folder")
	assert.Contains(t, mount.RecoveryAction, "delete its local folder")
	require.NotNil(t, mount.AutoRetry)
	assert.True(t, *mount.AutoRetry)
}

// Validates: R-6.6.25
func TestBuildStatusAccountsFromViews_NeverSyncedDriveIsNotUpToDate(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:alice@example.com")
	cfg := &config.Config{Drives: map[driveid.CanonicalID]config.Drive{cid: {SyncDir: "~/OneDrive"}}}
	views := []accountView{statusAccountView(authTestDisplayNameAlice, cid)}
	build := func() string {
		accounts := buildStatusAccountsFromViews(t.Context(), cfg, nil, views, &mockSyncStateQuerier{})
		require.Len(t, accounts, 1)
		require.Len(t, accounts[0].Drives, 1)
		return accounts[0].Drives[0].State
	}

	assert.Equal(t, driveStateNotSynced, build(), "status and drive list must agree on a drive that never synced")

	require.NoError(t, touchStateDBForAccount(t, cid))
	assert.Equal(t, driveStateReady, build())
}

// Validates: R-6.3.2
func TestNewStatusCmd_Structure(t *testing.T) {
	cmd := newStatusCmd()
	assert.Equal(t, "status", cmd.Name())
	assert.NotEmpty(t, cmd.Short)
	assert.NotNil(t, cmd.RunE)
	assert.Contains(t, cmd.Short, "drive")
	assert.Contains(t, cmd.Short, "shared folder")
	assert.Contains(t, cmd.Long, "configured accounts, drives, and shared folders")
	assert.Contains(t, cmd.Long, "--drive to filter")
	assert.Contains(t, cmd.Long, "--verbose")
}

// --- status accounts from the account views ---

// The account's names come from the catalog and its sign-in health from the
// saved login, through the account views.
func TestBuildStatusAccountsFromViews_SingleAccount(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:alice@example.com")
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{cid: {SyncDir: "~/OneDrive"}},
	}
	seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
		account.DisplayName = authTestDisplayNameAlice
	})
	seedConfiguredStatusDrives(t, cfg)
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_alice@example.com.json")

	accounts := statusAccountsThroughViews(t, cfg)

	require.Len(t, accounts, 1)
	acct := accounts[0]
	assert.Equal(t, "alice@example.com", acct.Email)
	assert.Equal(t, "personal", acct.DriveType)
	assert.Equal(t, authStateReady, acct.AuthState)
	assert.Equal(t, "Alice", acct.DisplayName)

	require.Len(t, acct.Drives, 1)
	assert.Equal(t, "~/OneDrive", acct.Drives[0].Folder)
	assert.Equal(t, driveStateNotSynced, acct.Drives[0].State)
}

// Drives are grouped under their account's email, and accounts are listed
// by email, through the account views.
func TestBuildStatusAccountsFromViews_MultiAccountGrouping(t *testing.T) {
	setTestDriveHome(t)

	paused := true
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"):   {SyncDir: "~/OneDrive"},
			driveid.MustCanonicalID("business:alice@example.com"):   {SyncDir: "~/Work"},
			driveid.MustCanonicalID("personal:bob@example.com"):     {SyncDir: "~/Bob", Paused: &paused},
			driveid.MustCanonicalID("business:charlie@example.com"): {SyncDir: "~/Charlie"},
		},
	}
	seedConfiguredStatusDrives(t, cfg)

	accounts := statusAccountsThroughViews(t, cfg)

	require.Len(t, accounts, 3)

	// Sorted alphabetically by email.
	assert.Equal(t, "alice@example.com", accounts[0].Email)
	assert.Len(t, accounts[0].Drives, 2)

	assert.Equal(t, "bob@example.com", accounts[1].Email)
	assert.Len(t, accounts[1].Drives, 1)
	assert.Equal(t, driveStatePaused, accounts[1].Drives[0].State)

	assert.Equal(t, "charlie@example.com", accounts[2].Email)
}

// An account whose saved login is missing or unreadable needs sign-in, read
// from the token file through the account views.
//
// Validates: R-2.10.47
func TestBuildStatusAccountsFromViews_AuthenticationRequiredStates(t *testing.T) {
	tests := []struct {
		name       string
		tokenFile  []byte // nil: no token file at all
		wantReason string
	}{
		{
			name:       "missing token",
			wantReason: string(authReasonMissingLogin),
		},
		{
			name:       "invalid saved login",
			tokenFile:  []byte("{invalid-json"),
			wantReason: string(authReasonInvalidSavedLogin),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setTestDriveHome(t)

			cid := driveid.MustCanonicalID("personal:alice@example.com")
			cfg := &config.Config{
				Drives: map[driveid.CanonicalID]config.Drive{cid: {SyncDir: "~/OneDrive"}},
			}
			seedConfiguredStatusDrives(t, cfg)
			if tc.tokenFile != nil {
				tokenPath := config.DriveTokenPath(cid)
				require.NoError(t, os.MkdirAll(filepath.Dir(tokenPath), 0o700))
				require.NoError(t, os.WriteFile(tokenPath, tc.tokenFile, 0o600))
			}

			accounts := statusAccountsThroughViews(t, cfg)

			require.Len(t, accounts, 1)
			assert.Equal(t, authStateAuthenticationNeeded, accounts[0].AuthState)
			assert.Equal(t, tc.wantReason, accounts[0].AuthReason)
			assert.Equal(t, driveStateNotSynced, accounts[0].Drives[0].State)
		})
	}
}

func TestBuildStatusAccountsFromViews_EmptySyncDir(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:alice@example.com")
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{cid: {SyncDir: ""}},
	}

	accounts := buildStatusAccountsFromViews(t.Context(), cfg, nil,
		[]accountView{statusAccountView("", cid)}, &mockSyncStateQuerier{})

	require.Len(t, accounts, 1)
	// An empty sync_dir does not change the drive's state: it has not synced.
	assert.Equal(t, driveStateNotSynced, accounts[0].Drives[0].State)
}

// A business account with SharePoint libraries is listed once, as the
// business account it signed in with, with its organization's name.
func TestBuildStatusAccountsFromViews_SharePointGrouping(t *testing.T) {
	setTestDriveHome(t)

	accountCID := driveid.MustCanonicalID("business:alice@contoso.com")
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			accountCID: {SyncDir: "~/Work"},
			driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"):   {SyncDir: "~/Marketing"},
			driveid.MustCanonicalID("sharepoint:alice@contoso.com:engineering:Wiki"): {SyncDir: "~/Eng"},
		},
	}
	seedCatalogAccount(t, accountCID, func(account *config.CatalogAccount) {
		account.DisplayName = authTestDisplayNameAlice
		account.OrgName = workflowTestOrgContoso
	})
	seedConfiguredStatusDrives(t, cfg)

	accounts := statusAccountsThroughViews(t, cfg)

	require.Len(t, accounts, 1)
	acct := accounts[0]
	assert.Equal(t, "alice@contoso.com", acct.Email)
	assert.Equal(t, "business", acct.DriveType) // business, not sharepoint
	assert.Equal(t, workflowTestOrgContoso, acct.OrgName)
	assert.Len(t, acct.Drives, 3)
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

func TestBuildStatusAccountsFromViews_PersonalDriveUsesAccountFacingName(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:alice@example.com")
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			cid: {SyncDir: "~/OneDrive", DisplayName: "My Home Drive"},
		},
	}

	accounts := buildStatusAccountsFromViews(t.Context(), cfg, nil,
		[]accountView{statusAccountView(authTestDisplayNameAlice, cid)}, &mockSyncStateQuerier{})

	require.Len(t, accounts, 1)
	assert.Equal(t, "Alice's personal OneDrive", accounts[0].Drives[0].Name)
}

// A paused drive stays paused when its account has no saved login, read
// through the account views.
func TestBuildStatusAccountsFromViews_PausedOverridesNoToken(t *testing.T) {
	setTestDriveHome(t)

	paused := true
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"): {SyncDir: "~/OneDrive", Paused: &paused},
		},
	}
	seedConfiguredStatusDrives(t, cfg)

	accounts := statusAccountsThroughViews(t, cfg)

	require.Len(t, accounts, 1)
	assert.Equal(t, driveStatePaused, accounts[0].Drives[0].State)
	assert.Equal(t, authStateAuthenticationNeeded, accounts[0].AuthState)
	assert.Equal(t, string(authReasonMissingLogin), accounts[0].AuthReason)
}

func TestBuildStatusAccountsFromViews_EmptyConfig(t *testing.T) {
	setTestDriveHome(t)

	accounts := statusAccountsThroughViews(t, &config.Config{Drives: map[driveid.CanonicalID]config.Drive{}})

	assert.Empty(t, accounts)
}

// --- sync state on the drive rows ---

func TestBuildStatusAccountsFromViews_SyncStatePopulated(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:alice@example.com")
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{cid: {SyncDir: "~/OneDrive"}},
	}

	syncState := &syncStateInfo{
		FileCount:      45,
		ConditionCount: 2,
	}

	accounts := buildStatusAccountsFromViews(t.Context(), cfg, nil,
		[]accountView{statusAccountView("", cid)},
		&mockSyncStateQuerier{state: syncState})

	require.Len(t, accounts, 1)
	require.Len(t, accounts[0].Drives, 1)
	require.NotNil(t, accounts[0].Drives[0].SyncState)

	ss := accounts[0].Drives[0].SyncState
	assert.Equal(t, 45, ss.FileCount)
	assert.Equal(t, 2, ss.ConditionCount)
}

func TestBuildStatusAccountsFromViews_NilSyncState(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:alice@example.com")
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{cid: {SyncDir: "~/OneDrive"}},
	}

	accounts := buildStatusAccountsFromViews(t.Context(), cfg, nil,
		[]accountView{statusAccountView("", cid)},
		&mockSyncStateQuerier{state: nil})

	require.Len(t, accounts, 1)
	assert.Nil(t, accounts[0].Drives[0].SyncState)
}

func TestQuerySyncState_NoDB(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)

	info := queryLiveSyncState(t.Context(), "/nonexistent/path/state.db", logger)
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

	info := queryLiveSyncState(t.Context(), dbPath, logger)
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

	db, err := sql.Open("sqlite", synctest.SQLiteFileURI(dbPath, nil))
	require.NoError(t, err)
	defer db.Close()

	ctx := t.Context()

	// Insert a baseline entry.
	_, err = db.ExecContext(ctx, `INSERT INTO baseline (path, item_id, parent_id, item_type)
		VALUES ('/test.txt', 'item1', 'root', 'file')`)
	require.NoError(t, err)

	require.NoError(t, db.Close())

	info := queryLiveSyncState(t.Context(), dbPath, logger)
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

	db, err := sql.Open("sqlite", synctest.SQLiteFileURI(dbPath, nil))
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

	store, err := syncstore.NewSyncStore(ctx, dbPath, logger)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, store.Close(ctx))
	}()

	_, err = store.RecordRetryWorkFailure(ctx, &syncdomain.RetryWorkFailure{
		Work: syncdomain.RetryWorkKey{
			Path:       "/x.txt",
			ActionType: syncdomain.ActionUpload,
		},
	}, func(int) time.Duration { return 0 })
	require.NoError(t, err)
	_, err = store.RecordRetryWorkFailure(ctx, &syncdomain.RetryWorkFailure{
		Work: syncdomain.RetryWorkKey{
			Path:       "/x.txt",
			ActionType: syncdomain.ActionUpload,
		},
	}, func(int) time.Duration { return 0 })
	require.NoError(t, err)
	_, err = store.RecordRetryWorkFailure(ctx, &syncdomain.RetryWorkFailure{
		Work: syncdomain.RetryWorkKey{
			Path:       "/x.txt",
			ActionType: syncdomain.ActionUpload,
		},
	}, func(int) time.Duration { return 0 })
	require.NoError(t, err)
	_, err = store.RecordRetryWorkFailure(ctx, &syncdomain.RetryWorkFailure{
		Work: syncdomain.RetryWorkKey{
			Path:       "/y.txt",
			ActionType: syncdomain.ActionUpload,
		},
	}, func(int) time.Duration { return 0 })
	require.NoError(t, err)
	_, err = store.RecordRetryWorkFailure(ctx, &syncdomain.RetryWorkFailure{
		Work: syncdomain.RetryWorkKey{
			Path:       "/y.txt",
			ActionType: syncdomain.ActionUpload,
		},
	}, func(int) time.Duration { return 0 })
	require.NoError(t, err)
	_, err = store.RecordRetryWorkFailure(ctx, &syncdomain.RetryWorkFailure{
		Work: syncdomain.RetryWorkKey{
			Path:       "/y.txt",
			ActionType: syncdomain.ActionUpload,
		},
	}, func(int) time.Duration { return 0 })
	require.NoError(t, err)
	_, err = store.RecordRetryWorkFailure(ctx, &syncdomain.RetryWorkFailure{
		Work: syncdomain.RetryWorkKey{
			Path:       "/y.txt",
			ActionType: syncdomain.ActionUpload,
		},
	}, func(int) time.Duration { return 0 })
	require.NoError(t, err)
	seedObservationIssueForStatusTest(t, store, &syncdomain.ObservationIssue{
		Path:      "/z.txt",
		DriveID:   testDriveID,
		IssueType: syncdomain.IssueInvalidFilename,
	})

	info := queryLiveSyncState(t.Context(), dbPath, logger)
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

	db, err := sql.Open("sqlite", synctest.SQLiteFileURI(dbPath, nil))
	require.NoError(t, err)
	defer db.Close()

	ctx := t.Context()
	store, err := syncstore.NewSyncStore(ctx, dbPath, logger)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, store.Close(ctx))
	}()

	scopeKey := syncdomain.SKPermRemoteWrite("Shared/Docs")
	require.NoError(t, store.UpsertBlockScope(ctx, &syncdomain.BlockScope{
		Key:           scopeKey,
		TrialInterval: time.Minute,
		NextTrialAt:   time.Unix(0, 0).UTC().Add(time.Minute),
	}))
	_, err = store.RecordBlockedRetryWork(ctx, syncdomain.RetryWorkKey{
		Path:       "/blocked/a.txt",
		ActionType: syncdomain.ActionUpload,
	}, scopeKey, nil)
	require.NoError(t, err)
	_, err = store.RecordBlockedRetryWork(ctx, syncdomain.RetryWorkKey{
		Path:       "/blocked/b.txt",
		ActionType: syncdomain.ActionUpload,
	}, scopeKey, nil)
	require.NoError(t, err)
	seedObservationIssueForStatusTest(t, store, &syncdomain.ObservationIssue{
		Path:      "/actionable.txt",
		DriveID:   driveid.New("test-drive-id"),
		IssueType: syncdomain.IssueInvalidFilename,
	})

	info := queryLiveSyncState(t.Context(), dbPath, logger)
	require.NotNil(t, info)
	assert.Equal(t, 3, info.ConditionCount)
}

// Validates: R-6.10.5
func TestQuerySyncState_UsesReadOnlyStatusSnapshotHelper(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	store, err := syncstore.NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)

	require.NoError(t, store.CommitMutation(t.Context(), &syncdomain.BaselineMutation{
		Action:   syncdomain.ActionDownload,
		Success:  true,
		Path:     "/tracked.txt",
		DriveID:  driveid.New("test-drive-id"),
		ItemID:   "item-1",
		ParentID: "root",
		ItemType: syncdomain.ItemTypeFile,
	}))

	walPath := dbPath + "-wal"
	shmPath := dbPath + "-shm"
	require.Eventually(t, func() bool {
		_, walErr := os.Stat(walPath)
		_, shmErr := os.Stat(shmPath)
		return walErr == nil && shmErr == nil
	}, eventWait, 10*time.Millisecond, "WAL sidecars were not created")

	require.NoError(t, os.Chmod(dbPath, 0o400))
	// #nosec G302 -- test intentionally makes the directory read-only to prove status stays on the read-only path.
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() {
		// #nosec G302 -- cleanup restores the tempdir so the writable store can close.
		assert.NoError(t, os.Chmod(dir, 0o700))
		assert.NoError(t, os.Chmod(dbPath, 0o600))
		assert.NoError(t, store.Close(context.Background()))
	})

	info := queryLiveSyncState(t.Context(), dbPath, logger)
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

	db, err := sql.Open("sqlite", synctest.SQLiteFileURI(dbPath, nil))
	require.NoError(t, err)
	defer db.Close()

	ctx := t.Context()
	store, err := syncstore.NewSyncStore(ctx, dbPath, logger)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, store.Close(ctx))
	}()

	scopeKey := syncdomain.SKPermRemoteWrite("Shared/Team Docs")
	seedObservationIssueForStatusTest(t, store, &syncdomain.ObservationIssue{
		Path:      "/invalid:name.txt",
		DriveID:   driveid.New("test-drive-id"),
		IssueType: syncdomain.IssueInvalidFilename,
	})
	require.NoError(t, store.UpsertBlockScope(ctx, &syncdomain.BlockScope{
		Key:           scopeKey,
		TrialInterval: time.Minute,
		NextTrialAt:   time.Unix(0, 0).UTC().Add(time.Minute),
	}))
	_, err = store.RecordBlockedRetryWork(ctx, syncdomain.RetryWorkKey{
		Path:       "/blocked/a.txt",
		ActionType: syncdomain.ActionUpload,
	}, scopeKey, nil)
	require.NoError(t, err)
	_, err = store.RecordBlockedRetryWork(ctx, syncdomain.RetryWorkKey{
		Path:       "/blocked/b.txt",
		ActionType: syncdomain.ActionUpload,
	}, scopeKey, nil)
	require.NoError(t, err)

	info := queryLiveSyncState(t.Context(), dbPath, logger)
	require.NotNil(t, info)
	invalidDescriptor := describeStatusCondition(syncdomain.ConditionInvalidFilename)
	sharedDescriptor := describeStatusCondition(syncdomain.ConditionRemoteWriteDenied)
	assert.ElementsMatch(t, []statusConditionJSON{
		{
			ConditionKey:  string(syncdomain.ConditionInvalidFilename),
			ConditionType: string(syncdomain.IssueInvalidFilename),
			Title:         invalidDescriptor.Title,
			Reason:        invalidDescriptor.Reason,
			Action:        invalidDescriptor.Action,
			Count:         1,
			Paths:         []string{"/invalid:name.txt"},
		},
		{
			ConditionKey:  string(syncdomain.ConditionRemoteWriteDenied),
			ConditionType: string(syncdomain.IssueRemoteWriteDenied),
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
			Drives: []statusDrive{
				{
					InternalID: "business:alice@example.com",
					Folder:     "~/Work",
					State:      driveStateReady,
					SyncState: &syncStateInfo{
						FileCount:      10,
						ConditionCount: 2,
						Conditions: []statusConditionJSON{
							{
								ConditionKey: string(syncdomain.ConditionQuotaExceeded),
								Title:        "Storage full",
								Count:        1,
								ScopeKind:    "drive",
								Scope:        "Shared/Docs",
							},
							{
								ConditionKey: string(syncdomain.ConditionQuotaExceeded),
								Title:        "Storage full",
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
	require.NoError(t, printStatusJSON(&buf, asBuiltStatusAccounts(accounts)))

	var result statusOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	require.Len(t, result.Accounts, 1)
	require.Len(t, result.Accounts[0].Drives, 1)
	require.Len(t, result.Accounts[0].Drives[0].SyncState.Conditions, 2)
	assert.Equal(t, "Shared/Docs", result.Accounts[0].Drives[0].SyncState.Conditions[0].Scope)
	assert.Equal(t, "Shared/Design", result.Accounts[0].Drives[0].SyncState.Conditions[1].Scope)
}

// Validates: R-2.10.32
func TestPrintSyncStateText_KeepsSameSummaryGroupsSeparatedByScope(t *testing.T) {
	t.Parallel()

	quotaDescriptor := describeStatusCondition(syncdomain.ConditionQuotaExceeded)
	ss := &syncStateInfo{
		ConditionCount: 2,
		Conditions: []statusConditionJSON{
			{
				ConditionKey:  string(syncdomain.ConditionQuotaExceeded),
				ConditionType: string(syncdomain.IssueQuotaExceeded),
				Title:         quotaDescriptor.Title,
				Reason:        quotaDescriptor.Reason,
				Action:        quotaDescriptor.Action,
				Count:         1,
				ScopeKind:     "drive",
				Scope:         "Shared/Docs",
			},
			{
				ConditionKey:  string(syncdomain.ConditionQuotaExceeded),
				ConditionType: string(syncdomain.IssueQuotaExceeded),
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
	require.NoError(t, printSyncStateText(&buf, "    ", ss))
	output := buf.String()
	assert.Equal(t, 2, strings.Count(output, "Storage full: 1 item"))
	assert.Contains(t, output, "Scope: Shared/Docs")
	assert.Contains(t, output, "Scope: Shared/Design")
}

func TestComputeSummary_Mixed(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			AuthState: authStateReady,
			Drives: []statusDrive{
				{State: driveStateReady, SyncState: &syncStateInfo{ConditionCount: 3}},
				{State: driveStatePaused},
			},
		},
		{
			AuthState: authStateAuthenticationNeeded,
			Drives: []statusDrive{
				{State: driveStateReady},
				{State: driveStateReady, SyncState: &syncStateInfo{ConditionCount: 1}},
			},
		},
	}

	s := newStatusOutput(asBuiltStatusAccounts(accounts)).Summary
	assert.Equal(t, 4, s.TotalDrives+s.TotalSharedFolders)
	assert.Equal(t, 1, s.Ready)
	assert.Equal(t, 2, s.Issues)
	assert.Equal(t, 1, s.Paused)
	assert.Equal(t, 1, s.AccountsRequiringAuth)
	assert.Equal(t, 4, s.TotalConditions)
}

func TestComputeSummary_IncludesNestedChildMounts(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			AuthState: authStateReady,
			Drives: []statusDrive{{
				State: driveStateReady,
				SharedFolders: []statusDrive{
					{State: driveStatePaused, SyncState: &syncStateInfo{ConditionCount: 2}},
				},
			}},
		},
	}

	s := newStatusOutput(asBuiltStatusAccounts(accounts)).Summary
	assert.Equal(t, 2, s.TotalDrives+s.TotalSharedFolders)
	assert.Equal(t, 1, s.Ready)
	assert.Equal(t, 1, s.Paused)
	assert.Equal(t, 2, s.TotalConditions)
}

func TestComputeSummary_Empty(t *testing.T) {
	t.Parallel()

	s := newStatusOutput(nil).Summary
	assert.Equal(t, 0, s.TotalDrives+s.TotalSharedFolders)
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
			Drives: []statusDrive{
				{
					InternalID: "personal:alice@example.com",
					Folder:     "~/OneDrive",
					State:      driveStateReady,
					SyncState:  &syncStateInfo{FileCount: 10, ConditionCount: 1},
				},
			},
		},
	}

	var buf bytes.Buffer
	err := printStatusJSON(&buf, asBuiltStatusAccounts(accounts))
	require.NoError(t, err)

	var result statusOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	require.Len(t, result.Accounts, 1)
	assert.Equal(t, "alice@example.com", result.Accounts[0].Email)
	assert.Equal(t, 1, result.Summary.Issues)
	assert.Equal(t, 1, result.Summary.TotalDrives)
	require.Len(t, result.Accounts[0].Drives, 1)
	assert.Equal(t, "~/OneDrive", result.Accounts[0].Drives[0].Folder)
}

// Validates: R-2.10.4
func TestPrintStatusJSON_WithConditions(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Email:     "alice@example.com",
			DriveType: "personal",
			AuthState: authStateReady,
			Drives: []statusDrive{
				{
					InternalID: "personal:alice@example.com",
					Folder:     "~/OneDrive",
					State:      driveStateReady,
					SyncState: &syncStateInfo{
						FileCount:      10,
						ConditionCount: 3,
						Conditions: []statusConditionJSON{
							{
								ConditionKey: string(syncdomain.ConditionQuotaExceeded),
								Title:        "Storage full",
								Count:        1,
								ScopeKind:    "drive",
								Scope:        "Shared/Team Docs",
							},
							{
								ConditionKey: string(syncdomain.ConditionInvalidFilename),
								Title:        "Invalid filename",
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
	require.NoError(t, printStatusJSON(&buf, asBuiltStatusAccounts(accounts)))

	var result statusOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	require.Len(t, result.Accounts, 1)
	require.Len(t, result.Accounts[0].Drives, 1)
	require.NotNil(t, result.Accounts[0].Drives[0].SyncState)
	require.Len(t, result.Accounts[0].Drives[0].SyncState.Conditions, 2)
	assert.Equal(t, "drive", result.Accounts[0].Drives[0].SyncState.Conditions[0].ScopeKind)
	assert.Equal(t, "Shared/Team Docs", result.Accounts[0].Drives[0].SyncState.Conditions[0].Scope)
}

// --- printStatusText ---

func TestPrintStatusText_NoMounts(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printStatusText(&buf, nil))

	output := buf.String()
	assert.Empty(t, output)
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
			Drives: []statusDrive{
				{
					InternalID: "business:alice@contoso.com",
					Folder:     "~/Work",
					State:      driveStateReady,
				},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printStatusText(&buf, asBuiltStatusAccounts(accounts)))

	output := buf.String()
	assert.True(t, strings.HasPrefix(output, "Account: Alice Smith <alice@contoso.com>\n"))
	assert.Contains(t, output, "Organization: Contoso")
	assert.NotContains(t, output, "Auth:")
	assert.Contains(t, output, "~/Work")
}

func TestPrintStatusText_HealthySingleDriveIsUserFacing(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Email:       "user@example.com",
			DisplayName: "Test User",
			AuthState:   authStateReady,
			Drives: []statusDrive{
				{
					Kind:   statusDriveKindPersonalOneDrive,
					Name:   "Test User's personal OneDrive",
					Folder: "~/OneDrive",
					State:  driveStateReady,
					Storage: &statusStorage{
						UsedBytes:  135 * 1024,
						TotalBytes: 5 * 1024 * 1024 * 1024,
						Used:       "135.0 KB",
						Total:      "5.0 GB",
					},
					SyncState: &syncStateInfo{FileCount: 7},
				},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printStatusText(&buf, asBuiltStatusAccounts(accounts)))

	assert.Equal(t, ""+
		"Account: Test User <user@example.com>\n"+
		"  Test User's personal OneDrive\n"+
		"    Folder: ~/OneDrive\n"+
		"    Storage: 135.0 KB of 5.0 GB used\n"+
		"    Status: Up to date\n"+
		"    Files: 7\n",
		buf.String(),
	)
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
			Drives: []statusDrive{
				{
					InternalID: "personal:alice@example.com",
					Folder:     "~/OneDrive",
					State:      driveStateReady,
				},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printStatusText(&buf, asBuiltStatusAccounts(accounts)))

	output := buf.String()
	assert.Contains(t, output, "Sign-in required:")
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
			Drives: []statusDrive{
				{
					InternalID: "personal:bob@example.com",
					Folder:     "~/OneDrive",
					State:      driveStateReady,
					SyncState:  &syncStateInfo{},
				},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printStatusText(&buf, asBuiltStatusAccounts(accounts)))

	output := buf.String()
	assert.NotContains(t, output, "No active conditions.")
	assert.NotContains(t, output, "Issues:")
}

func TestPrintStatusText_EmptySyncDir(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Email:     "bob@example.com",
			DriveType: "personal",
			AuthState: authStateReady,
			Drives: []statusDrive{
				{
					InternalID: "personal:bob@example.com",
					Folder:     "",
					State:      driveStateReady,
				},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printStatusText(&buf, asBuiltStatusAccounts(accounts)))

	output := buf.String()
	assert.Contains(t, output, syncDirNotSet)
}

func TestPrintSummaryText_AllStates(t *testing.T) {
	t.Parallel()

	s := statusSummary{
		TotalDrives:           4,
		Ready:                 3,
		Paused:                1,
		AccountsRequiringAuth: 1,
		TotalConditions:       3,
	}

	var buf bytes.Buffer
	require.NoError(t, printSummaryText(&buf, &s))

	output := buf.String()
	assert.Contains(t, output, "4 drives")
	assert.Contains(t, output, "3 up to date")
	assert.Contains(t, output, "1 paused")
	assert.Contains(t, output, "1 account needs sign-in")
	assert.NotContains(t, output, "conditions")
}

// Every count of the summary line agrees with its number, the verb of the
// sign-in count included.
//
// Validates: R-2.10.4
func TestPrintSummaryText_CountsAgreeWithTheirNumbers(t *testing.T) {
	t.Parallel()

	var one bytes.Buffer
	require.NoError(t, printSummaryText(&one, &statusSummary{
		TotalDrives: 1, TotalSharedFolders: 1, Ready: 1, AccountsRequiringAuth: 1,
	}))
	assert.Equal(t, "Status: 1 drive, 1 shared folder: 1 up to date, 1 account needs sign-in\n", one.String())

	var two bytes.Buffer
	require.NoError(t, printSummaryText(&two, &statusSummary{
		TotalDrives: 2, TotalSharedFolders: 2, Syncing: 2, Pending: 2, Issues: 2, Unavailable: 2, AccountsRequiringAuth: 2,
	}))
	assert.Equal(t, "Status: 2 drives, 2 shared folders: 2 syncing, 2 pending, 2 with issues, 2 unavailable, "+
		"2 accounts need sign-in\n", two.String())
}

func TestPrintSummaryText_WithPendingAndRetrying(t *testing.T) {
	t.Parallel()

	s := statusSummary{
		TotalDrives:      2,
		Ready:            2,
		TotalConditions:  1,
		TotalRemoteDrift: 5,
		TotalRetrying:    3,
	}

	var buf bytes.Buffer
	require.NoError(t, printSummaryText(&buf, &s))

	output := buf.String()
	assert.Contains(t, output, "2 drives")
	assert.NotContains(t, output, "remote drift")
	assert.NotContains(t, output, "retrying")
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
	require.NoError(t, printSyncStateText(&buf, "    ", ss))

	output := buf.String()
	assert.Contains(t, output, "Remote changes: 3 items")
	assert.Contains(t, output, "Retrying: 2 items")
}

// Validates: R-2.10.4
func TestPrintSyncStateText_WithConditions(t *testing.T) {
	t.Parallel()

	quotaDescriptor := describeStatusCondition(syncdomain.ConditionQuotaExceeded)
	invalidDescriptor := describeStatusCondition(syncdomain.ConditionInvalidFilename)
	authDescriptor := describeStatusCondition(syncdomain.ConditionAuthenticationRequired)
	ss := &syncStateInfo{
		ConditionCount: 3,
		Retrying:       2,
		Conditions: []statusConditionJSON{
			{
				ConditionKey:  string(syncdomain.ConditionQuotaExceeded),
				ConditionType: string(syncdomain.IssueQuotaExceeded),
				Title:         quotaDescriptor.Title,
				Reason:        quotaDescriptor.Reason,
				Action:        quotaDescriptor.Action,
				Count:         1,
				ScopeKind:     "drive",
				Scope:         "Shared/Team Docs",
				Paths:         []string{"/quota/a.txt"},
			},
			{
				ConditionKey:  string(syncdomain.ConditionInvalidFilename),
				ConditionType: string(syncdomain.IssueInvalidFilename),
				Title:         invalidDescriptor.Title,
				Reason:        invalidDescriptor.Reason,
				Action:        invalidDescriptor.Action,
				Count:         2,
				Paths:         []string{"/bad:name.txt", "/worse:name.txt"},
			},
			{
				ConditionKey:  string(syncdomain.ConditionAuthenticationRequired),
				ConditionType: string(syncdomain.IssueUnauthorized),
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
	require.NoError(t, printSyncStateText(&buf, "    ", ss))
	requireGoldenText(t, "status_sync_state_with_conditions.golden", buf.String())
}

func requireSingleStatusDriveJSON(
	t *testing.T,
	decoded *statusOutput,
	canonicalID string,
) (statusDrive, *syncStateInfo) {
	t.Helper()

	drive, syncState := findStatusDriveJSON(t, decoded, canonicalID)
	require.Equal(t, 1, decoded.Summary.TotalDrives, "expected filtered status output")
	return drive, syncState
}

func findStatusDriveJSON(
	t *testing.T,
	decoded *statusOutput,
	canonicalID string,
) (statusDrive, *syncStateInfo) {
	t.Helper()
	require.NotNil(t, decoded)

	var (
		foundDrive statusDrive
		firstDrive statusDrive
		found      bool
		count      int
	)
	for i := range decoded.Accounts {
		for j := range decoded.Accounts[i].Drives {
			count++
			if count == 1 {
				firstDrive = decoded.Accounts[i].Drives[j]
			}
			drive, ok := findStatusMountRecursive(&decoded.Accounts[i].Drives[j], canonicalID)
			if ok {
				require.False(t, found, "expected exactly one drive in filtered status output")
				foundDrive = drive
				found = true
			}
		}
	}
	if !found && count == 1 {
		return firstDrive, firstDrive.SyncState
	}
	require.True(t, found, "expected drive %s in status output", canonicalID)
	return foundDrive, foundDrive.SyncState
}

func findStatusMountRecursive(mount *statusDrive, identity string) (statusDrive, bool) {
	if mount == nil {
		return statusDrive{}, false
	}
	if mount.InternalID == identity {
		return *mount, true
	}
	if identity == "" {
		return *mount, true
	}
	for i := range mount.SharedFolders {
		if found, ok := findStatusMountRecursive(&mount.SharedFolders[i], identity); ok {
			return found, true
		}
	}

	return statusDrive{}, false
}

// seedDamagedStatusDrive configures one drive whose state database the test
// then damages; it returns the drive's canonical ID and the config path.
func seedDamagedStatusDrive(t *testing.T) (driveid.CanonicalID, string) {
	t.Helper()

	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:damaged@example.com")
	require.NoError(t, config.AppendDriveSection(t.Context(), cfgPath, cid, "~/OneDrive"))
	seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
		account.DisplayName = "Damaged User"
	})
	seedCatalogDrive(t, cid, func(drive *config.CatalogDrive) {
		drive.RemoteDriveID = "drive-damaged"
	})
	require.NoError(t, os.MkdirAll(filepath.Dir(config.DriveStatePath(cid)), 0o700))

	return cid, cfgPath
}

// runStatusJSONForDrive runs `status --json --drive <cid>` and returns the
// one drive it rendered.
func runStatusJSONForDrive(t *testing.T, cfgPath string, cid driveid.CanonicalID) (statusDrive, *syncStateInfo, string) {
	t.Helper()

	var out bytes.Buffer
	var logBuf bytes.Buffer
	cc := newCommandContext(&out, cfgPath)
	cc.Logger = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cc.Flags.Drive = []string{cid.String()}
	cc.Flags.JSON = true

	require.NoError(t, runStatusCommand(t.Context(), cc, false))

	var decoded statusOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	drive, syncState := requireSingleStatusDriveJSON(t, &decoded, cid.String())

	return drive, syncState, logBuf.String()
}

func assertStatusDriveSyncStateUnreadable(t *testing.T, drive *statusDrive, syncState *syncStateInfo, logs string) {
	t.Helper()

	assert.Equal(t, driveStateUnavailable, drive.State, "an unreadable state database is not 'up to date'")
	assert.Equal(t, syncStateUnreadableReason, drive.StateReason)
	assert.Equal(t, syncStateUnreadableDetail, drive.StateDetail)
	assert.Equal(t, syncStateUnreadableAction, drive.RecoveryAction)
	require.NotNil(t, syncState)
	assert.NotEmpty(t, syncState.ReadError, "the sync state says why it could not be read")
	assert.Zero(t, syncState.FileCount)
	assert.Equal(t, 1, strings.Count(logs, "cannot read the sync state for this drive"), "one Warn per drive")
	assert.Contains(t, logs, "level=WARN")
}

// Validates: R-6.6.25, R-6.6.28
//
// A state database that exists but is not SQLite used to collapse to an empty
// sync snapshot: "Up to date", zero files, no issues. It is unavailable.
func TestStatusCommand_UnreadableStateStoreIsReportedAsUnavailable(t *testing.T) {
	cid, cfgPath := seedDamagedStatusDrive(t)
	require.NoError(t, os.WriteFile(config.DriveStatePath(cid), []byte("not a sqlite database"), 0o600))

	drive, syncState, logs := runStatusJSONForDrive(t, cfgPath, cid)
	assertStatusDriveSyncStateUnreadable(t, &drive, syncState, logs)
}

// Validates: R-6.6.25, R-6.6.28
//
// The existence check itself failing (here a symlink loop; a permission
// problem on the data directory behaves the same) used to read as "no state
// database": a drive that had synced for months showed "Not synced yet".
func TestStatusCommand_UncheckableStateDatabaseIsReportedAsUnavailable(t *testing.T) {
	cid, cfgPath := seedDamagedStatusDrive(t)
	statePath := config.DriveStatePath(cid)
	require.NoError(t, os.Symlink(filepath.Base(statePath), statePath))

	drive, syncState, logs := runStatusJSONForDrive(t, cfgPath, cid)
	assertStatusDriveSyncStateUnreadable(t, &drive, syncState, logs)
	assert.NotEqual(t, driveStateNotSynced, drive.State)
}

// Validates: R-6.6.25, R-6.6.28
//
// A state database this user may not read (fs.ErrPermission) is unavailable,
// not a drive with nothing to report.
func TestStatusCommand_PermissionDeniedStateDatabaseIsReportedAsUnavailable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files regardless of their mode")
	}

	cid, cfgPath := seedDamagedStatusDrive(t)
	statePath := config.DriveStatePath(cid)
	createTestStateDB(t, statePath)
	require.NoError(t, os.Chmod(statePath, 0o000))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(statePath, 0o600)) })

	drive, syncState, logs := runStatusJSONForDrive(t, cfgPath, cid)
	assertStatusDriveSyncStateUnreadable(t, &drive, syncState, logs)
	assert.Contains(t, syncState.ReadError, statePath)
}

// Validates: R-6.6.28
//
// A data directory this user may not read is reported as that, naming the
// directory: status fails instead of printing "No accounts configured" or a
// drive that is "Not synced yet". Every managed read opens the directory
// through fsroot, so the catalog load is what reports it first; discovery's
// own read error (TestDiscoverCIDFiles_ReadDirError) is the backstop behind
// it.
func TestStatusCommand_UnreadableDataDirIsReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads directories regardless of their mode")
	}

	cid, cfgPath := seedDamagedStatusDrive(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.Chmod(dataDir, 0o311))                      //nolint:gosec // execute-only on purpose: the test needs an unlistable directory
	t.Cleanup(func() { assert.NoError(t, os.Chmod(dataDir, 0o700)) }) //nolint:gosec // restores the private test directory

	var out bytes.Buffer
	cc := newCommandContext(&out, cfgPath)
	cc.Flags.Drive = []string{cid.String()}

	err := runStatusCommand(t.Context(), cc, false)
	require.ErrorIs(t, err, fs.ErrPermission)
	assert.Contains(t, err.Error(), dataDir)
	assert.NotContains(t, out.String(), "No accounts configured")
	assert.NotContains(t, out.String(), "Not synced yet")
}

// Validates: R-6.6.28
//
// A token file that cannot be checked is not a missing login: signing in again
// would write to the same unreadable place, so the reason tells the user to
// look at the data directory instead.
func TestInspectSavedLogin_UnreadableTokenIsNotMissingLogin(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cid := driveid.MustCanonicalID("personal:alice@example.com")
	tokenPath := config.DriveTokenPath(cid)
	require.NoError(t, os.MkdirAll(filepath.Dir(tokenPath), 0o700))
	require.NoError(t, os.Symlink(filepath.Base(tokenPath), tokenPath))

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	state := inspectSavedLogin(t.Context(), "alice@example.com", []driveid.CanonicalID{cid}, logger)
	assert.Equal(t, authReasonSavedLoginUnreadable, state)
	assert.Contains(t, logBuf.String(), "level=WARN")

	health := evaluateAccountViewAuth(state, "")
	assert.Equal(t, authStateAuthenticationNeeded, health.State)
	assert.NotEqual(t, authAction(authReasonMissingLogin), health.Action)
	assert.Contains(t, health.Action, "data directory")
}

func TestComputeSummary_AggregatesPendingAndRetrying(t *testing.T) {
	t.Parallel()

	accounts := []statusAccount{
		{
			Drives: []statusDrive{
				{State: driveStateReady, SyncState: &syncStateInfo{RemoteDrift: 3, Retrying: 1}},
				{State: driveStateReady, SyncState: &syncStateInfo{RemoteDrift: 2, Retrying: 4}},
			},
		},
	}

	s := newStatusOutput(asBuiltStatusAccounts(accounts)).Summary
	assert.Equal(t, 5, s.TotalRemoteDrift)
	assert.Equal(t, 5, s.TotalRetrying)
}

// createTestStateDB creates a minimal SQLite DB with tables matching the sync schema.
func createTestStateDB(t *testing.T, dbPath string) {
	t.Helper()

	store, err := syncstore.NewSyncStore(t.Context(), dbPath, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	require.NoError(t, store.Close(t.Context()))
}

// Validates: R-3.5.1, R-4.1
//
// A drive that relies on the default sync_dir has an empty sync_dir in config.
// Reporting that raw value would tell the user their files sync nowhere; the
// live TestE2E_FileOps_RoundTrip/status_json checks the same on an account.
func TestBuildConfiguredStatusDrive_ReportsDefaultSyncDirWhenUnset(t *testing.T) {
	t.Parallel()

	cid := driveid.MustCanonicalID("personal:user@example.com")
	cfg := config.DefaultConfig()
	cfg.Drives = map[driveid.CanonicalID]config.Drive{cid: {}}

	drive := cfg.Drives[cid]
	got := buildConfiguredStatusDrive(t.Context(), cfg, cid, &drive, &statusAccount{}, nil)

	assert.NotEmpty(t, got.Folder, "status must report the effective sync dir, not the raw configured value")
	assert.Equal(t,
		config.EffectiveSyncDir(cfg, cid, "", slog.New(slog.DiscardHandler)),
		got.Folder,
	)
}

// Validates: R-3.5.1, R-4.1
func TestBuildConfiguredStatusDrive_PrefersConfiguredSyncDir(t *testing.T) {
	t.Parallel()

	cid := driveid.MustCanonicalID("personal:user@example.com")
	cfg := config.DefaultConfig()
	cfg.Drives = map[driveid.CanonicalID]config.Drive{cid: {SyncDir: "/explicit/path"}}

	drive := cfg.Drives[cid]
	got := buildConfiguredStatusDrive(t.Context(), cfg, cid, &drive, &statusAccount{}, nil)

	assert.Equal(t, "/explicit/path", got.Folder)
}

// Validates: R-4.8.3, R-4.8.4
//
// A drive whose sync folder holds the data folder is refused by sync, and
// status says so before sync runs: one warning on the console naming the
// config file, the drive and both folders, whether the sync folder is the
// drive's default (~/OneDrive here) or configured.
func TestStatusCommand_WarnsOnceAboutASyncDirHoldingTheDataFolder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		section string // the drive's keys in config.toml
	}{
		{name: "default sync_dir"},
		{name: "configured sync_dir", section: "sync_dir = \"~/OneDrive\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setTestDriveHome(t)
			home, err := os.UserHomeDir()
			require.NoError(t, err)
			t.Setenv("XDG_DATA_HOME", filepath.Join(home, "OneDrive", "data"))
			cid := driveid.MustCanonicalID("personal:status-warning@example.com")
			seedCatalogAccount(t, cid, nil)
			seedCatalogDrive(t, cid, nil)
			cfgPath := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(cfgPath, []byte("[\""+cid.String()+"\"]\n"+tc.section), 0o600))
			resolvedHome, err := filepath.EvalSymlinks(home)
			require.NoError(t, err)
			dataDir, err := filepath.EvalSymlinks(config.DefaultDataDir())
			require.NoError(t, err)

			var out, console bytes.Buffer
			cc := newCommandContext(&out, cfgPath)
			cc.Logger = slog.New(newConsoleLogHandler(&console, slog.LevelWarn, consoleLogHuman))
			require.NoError(t, runStatusCommand(t.Context(), cc, false))

			warning := "Warning: " + cfgPath + `: drive "` + cid.String() + `" sync_dir ` +
				filepath.Join(resolvedHome, "OneDrive") + " contains " + dataDir + ", "
			assert.Equal(t, 1, strings.Count(console.String(), warning), console.String())
		})
	}
}

// Validates: R-3.1.6
//
// mount_ref is the only identity status JSON publishes. It must identify a
// mount stably while revealing none of the things R-3.1.6 keeps private: the
// account email, the canonical/mount ID, and for shortcut children the remote
// drive and item IDs.
func TestStatusJSON_MountRefIsOpaqueAndDoesNotLeakIdentifiers(t *testing.T) {
	t.Parallel()

	const (
		email        = "alice@example.com"
		parentMount  = "personal:" + email
		remoteItemID = "F1DA660E69BDEC82!s7407aa71fc804d818f125b99de2ab146"
	)

	childMount := config.ChildMountID(parentMount, remoteItemID)

	var buf bytes.Buffer
	require.NoError(t, printStatusJSON(&buf, []statusAccount{{
		Email: email,
		Drives: []statusDrive{{
			InternalID: parentMount,
			MountRef:   config.MountRef(parentMount),
			Kind:       statusDriveKindPersonalOneDrive,
			Name:       "personal",
			State:      driveStateReady,
			SharedFolders: []statusDrive{{
				InternalID: childMount,
				MountRef:   config.MountRef(childMount),
				Kind:       statusDriveKindSharedFolder,
				Name:       "shared",
				State:      driveStateReady,
			}},
		}},
	}}))

	out := buf.String()

	assert.Contains(t, out, config.MountRef(parentMount))
	assert.Contains(t, out, config.MountRef(childMount))

	// The email is legitimately published at the account level, so only the
	// identifier forms R-3.1.6 names are checked here.
	assert.NotContains(t, out, parentMount, "canonical/mount ID must not appear")
	assert.NotContains(t, out, childMount, "child mount ID must not appear")
	assert.NotContains(t, out, remoteItemID, "remote item ID must not appear")
	assert.NotContains(t, out, "binding:", "mount binding must not appear")
}

// Validates: R-3.1.6
func TestStatusJSON_MountRefDistinguishesSiblingChildren(t *testing.T) {
	t.Parallel()

	parent := "personal:alice@example.com"
	first := config.ChildMountID(parent, "DRIVE!s1")
	second := config.ChildMountID(parent, "DRIVE!s2")

	assert.NotEqual(t, config.MountRef(first), config.MountRef(second),
		"two shortcut children under one parent must be distinguishable")
}

// Validates: R-2.10.1
// Without --verbose or --debug, status drops the raw diagnostic behind every
// shared folder row, however deeply it is nested, and keeps the plain reason.
func TestDropStatusDebugDetail_ClearsNestedRawDetailOnly(t *testing.T) {
	t.Parallel()

	child := statusDrive{StateDetail: "plain reason", StateDebugDetail: "raw: chain"}
	accounts := []statusAccount{{
		Drives: []statusDrive{{
			StateDebugDetail: "raw: drive",
			SharedFolders:    []statusDrive{child, child},
		}},
	}}

	dropStatusDebugDetail(accounts)

	drive := accounts[0].Drives[0]
	assert.Empty(t, drive.StateDebugDetail)
	assert.Empty(t, drive.SharedFolders[0].StateDebugDetail)
	assert.Equal(t, "plain reason", drive.SharedFolders[0].StateDetail)
	assert.Empty(t, drive.SharedFolders[1].StateDebugDetail)
}
