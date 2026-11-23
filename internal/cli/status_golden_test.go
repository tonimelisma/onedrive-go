package cli

import (
	"bytes"
	"testing"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"

	"github.com/stretchr/testify/require"
)

func goldenStatusAccounts() []statusAccount {
	invalidDescriptor := describeStatusCondition(syncdomain.ConditionInvalidFilename)
	authDescriptor := describeStatusCondition(syncdomain.ConditionAuthenticationRequired)
	sharedDescriptor := describeStatusCondition(syncdomain.ConditionRemoteWriteDenied)

	return asBuiltStatusAccounts([]statusAccount{
		{
			Email:       "alice@example.com",
			DriveType:   "personal",
			AuthState:   authStateReady,
			DisplayName: "Alice Example",
			OrgName:     "Contoso",
			Drives: []statusDrive{
				{
					// A realistic mount ID so the golden shows a real mount_ref
					// digest rather than an empty string.
					InternalID: "personal:alice@example.com",
					MountRef:   config.MountRef("personal:alice@example.com"),
					Kind:       statusDriveKindPersonalOneDrive,
					Name:       "Alice Example's personal OneDrive",
					Folder:     "/Users/alice/OneDrive",
					State:      driveStateReady,
					SyncState: &syncStateInfo{
						FileCount:      42,
						ConditionCount: 3,
						RemoteDrift:    2,
						Retrying:       1,
						ExamplesLimit:  defaultVisiblePaths,
						Conditions: []statusConditionJSON{
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
								ConditionKey:  string(syncdomain.ConditionAuthenticationRequired),
								ConditionType: string(syncdomain.IssueUnauthorized),
								Title:         authDescriptor.Title,
								Reason:        authDescriptor.Reason,
								Action:        authDescriptor.Action,
								Count:         1,
								ScopeKind:     "account",
								Scope:         "your OneDrive account authorization",
								Paths:         nil,
							},
							{
								ConditionKey:  string(syncdomain.ConditionRemoteWriteDenied),
								ConditionType: string(syncdomain.IssueRemoteWriteDenied),
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
					InternalID: "business:bob@example.com",
					MountRef:   config.MountRef("business:bob@example.com"),
					Kind:       statusDriveKindBusinessOneDrive,
					Name:       "OneDrive - Example Corp",
					Folder:     "/Users/bob/WorkDrive",
					State:      driveStatePaused,
				},
			},
		},
	})
}

// goldenBlockedSharedFolder is a removed shared folder whose local cleanup
// is blocked, carrying the raw diagnostic that only --verbose shows.
func goldenBlockedSharedFolder() statusDrive {
	metadata := syncshortcut.ShortcutRootStatus(syncshortcut.ShortcutRootStateRemovedCleanupBlocked)
	autoRetry := metadata.AutoRetry
	return statusDrive{
		InternalID:           "personal:alice@example.com|binding-docs",
		MountRef:             config.MountRef("personal:alice@example.com|binding-docs"),
		NamespaceID:          "personal:alice@example.com",
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
	require.NoError(t, printStatusText(&buf, goldenDefaultStatusAccounts()))
	assertGoldenFile(t, "status_text.golden", buf.Bytes())
}

// Validates: R-2.14.3, R-2.10.1
// Under --verbose or --debug a shared folder row adds the raw diagnostic
// behind its plain reason.
func TestStatusOutputGoldenTextVerbose(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printStatusText(&buf, goldenStatusAccounts()))
	assertGoldenFile(t, "status_text_verbose.golden", buf.Bytes())
}

// Validates: R-2.14.3
func TestStatusOutputGoldenJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printStatusJSON(&buf, goldenDefaultStatusAccounts()))
	assertGoldenFile(t, "status_json.golden", buf.Bytes())
}
