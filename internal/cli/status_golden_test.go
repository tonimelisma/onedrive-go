package cli

import (
	"bytes"
	"testing"

	"github.com/tonimelisma/onedrive-go/internal/config"

	"github.com/stretchr/testify/require"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func goldenStatusAccounts() []statusAccount {
	invalidDescriptor := describeStatusCondition(syncengine.ConditionInvalidFilename)
	authDescriptor := describeStatusCondition(syncengine.ConditionAuthenticationRequired)
	sharedDescriptor := describeStatusCondition(syncengine.ConditionRemoteWriteDenied)

	return []statusAccount{
		{
			Email:       "alice@example.com",
			DriveType:   "personal",
			AuthState:   authStateReady,
			DisplayName: "Alice Example",
			OrgName:     "Contoso",
			Drives: []statusDrive{
				{
					// A realistic MountID so the golden shows a real mount_ref
					// digest rather than an empty string.
					MountID:  "personal:alice@example.com",
					MountRef: config.MountRef("personal:alice@example.com"),
					Kind:     statusDriveKindPersonalOneDrive,
					Name:     "Alice Example's personal OneDrive",
					Folder:   "/Users/alice/OneDrive",
					State:    driveStateReady,
					SyncState: &syncStateInfo{
						FileCount:      42,
						ConditionCount: 3,
						RemoteDrift:    2,
						Retrying:       1,
						ExamplesLimit:  defaultVisiblePaths,
						Conditions: []statusConditionJSON{
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
								ConditionKey:  string(syncengine.ConditionAuthenticationRequired),
								ConditionType: string(syncengine.IssueUnauthorized),
								Title:         authDescriptor.Title,
								Reason:        authDescriptor.Reason,
								Action:        authDescriptor.Action,
								Count:         1,
								ScopeKind:     "account",
								Scope:         "your OneDrive account authorization",
								Paths:         nil,
							},
							{
								ConditionKey:  string(syncengine.ConditionRemoteWriteDenied),
								ConditionType: string(syncengine.IssueRemoteWriteDenied),
								Title:         sharedDescriptor.Title,
								Reason:        sharedDescriptor.Reason,
								Action:        sharedDescriptor.Action,
								Count:         1,
								ScopeKind:     "directory",
								Scope:         "Shared/Docs",
								Paths:         []string{"Shared/Docs/report.docx"},
							},
						},
					},
					SharedFolders: []statusDrive{goldenBlockedSharedFolder()},
				},
			},
		},
		{
			Email:       "bob@example.com",
			DisplayName: "Bob Example",
			OrgName:     "Example Corp",
			DriveType:   "business",
			AuthState:   authStateAuthenticationNeeded,
			AuthReason:  string(authReasonInvalidSavedLogin),
			AuthAction:  authAction(authReasonInvalidSavedLogin),
			Drives: []statusDrive{
				{
					MountID:  "business:bob@example.com",
					MountRef: config.MountRef("business:bob@example.com"),
					Kind:     statusDriveKindBusinessOneDrive,
					Name:     "OneDrive - Example Corp",
					Folder:   "/Users/bob/WorkDrive",
					State:    driveStatePaused,
				},
			},
		},
	}
}

// goldenBlockedSharedFolder is a removed shared folder whose local cleanup
// is blocked, carrying the raw diagnostic that only --verbose shows.
func goldenBlockedSharedFolder() statusDrive {
	metadata := syncengine.ShortcutRootStatus(syncengine.ShortcutRootStateRemovedCleanupBlocked)
	autoRetry := metadata.AutoRetry
	return statusDrive{
		MountID:              "personal:alice@example.com|binding-docs",
		MountRef:             config.MountRef("personal:alice@example.com|binding-docs"),
		NamespaceID:          "personal:alice@example.com",
		ProjectionKind:       statusProjectionChild,
		Kind:                 statusDriveKindSharedFolder,
		Name:                 "Docs",
		Folder:               "/Users/alice/OneDrive/Shared/Docs",
		State:                metadata.DisplayState,
		StateLabel:           metadata.StatusLabel,
		StateReason:          metadata.StateReason,
		IssueClass:           string(metadata.IssueClass),
		StateDetail:          metadata.Issue,
		StateDebugDetail:     "unsafe path: the shared folder's local path is not a directory",
		RecoveryClass:        string(metadata.RecoveryClass),
		RecoveryAction:       metadata.RecoveryAction,
		AutoRetry:            &autoRetry,
		ProtectedCurrentPath: "/Users/alice/OneDrive/Shared/Docs",
	}
}

// goldenDefaultStatusAccounts is what status shows without --verbose or
// --debug: the raw diagnostics are dropped before rendering.
func goldenDefaultStatusAccounts() []statusAccount {
	accounts := goldenStatusAccounts()
	dropStatusDebugDetail(accounts)
	return accounts
}

// Validates: R-2.14.3
func TestStatusOutputGoldenText(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printStatusText(&buf, goldenDefaultStatusAccounts(), false))
	assertGoldenFile(t, "status_text.golden", buf.Bytes())
}

// Validates: R-2.14.3, R-2.10.1
// Under --verbose or --debug a shared folder row adds the raw diagnostic
// behind its plain reason.
func TestStatusOutputGoldenTextVerbose(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printStatusText(&buf, goldenStatusAccounts(), false))
	assertGoldenFile(t, "status_text_verbose.golden", buf.Bytes())
}

// Validates: R-2.14.3
func TestStatusOutputGoldenJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printStatusJSON(&buf, goldenDefaultStatusAccounts()))
	assertGoldenFile(t, "status_json.golden", buf.Bytes())
}
