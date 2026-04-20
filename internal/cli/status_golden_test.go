package cli

import (
	"bytes"
	"testing"

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
					CanonicalID: "personal:alice@example.com",
					DisplayName: "Alice Personal",
					SyncDir:     "/Users/alice/OneDrive",
					State:       driveStateReady,
					SyncState: &syncStateInfo{
						LastSyncTime:     "2026-04-03T10:30:00Z",
						LastSyncDuration: "1500",
						FileCount:        42,
						ConditionCount:   3,
						RemoteDrift:      2,
						Retrying:         1,
						LastError:        "sync: network timeout",
						ExamplesLimit:    defaultVisiblePaths,
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
				},
			},
		},
		{
			Email:      "bob@example.com",
			DriveType:  "business",
			AuthState:  authStateAuthenticationNeeded,
			AuthReason: string(authReasonInvalidSavedLogin),
			AuthAction: authAction(authReasonInvalidSavedLogin),
			Drives: []statusDrive{
				{
					CanonicalID: "business:bob@example.com",
					DisplayName: "Bob Work",
					SyncDir:     "/Users/bob/WorkDrive",
					State:       driveStatePaused,
				},
			},
		},
	}
}

// Validates: R-2.14.3
func TestStatusOutputGoldenText(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printStatusText(&buf, goldenStatusAccounts(), false))
	assertGoldenFile(t, "status_text.golden", buf.Bytes())
}

// Validates: R-2.14.3
func TestStatusOutputGoldenJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printStatusJSON(&buf, goldenStatusAccounts()))
	assertGoldenFile(t, "status_json.golden", buf.Bytes())
}
