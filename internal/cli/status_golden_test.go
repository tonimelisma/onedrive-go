package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func goldenStatusAccounts() []statusAccount {
	invalidDescriptor := synctypes.DescribeSummary(synctypes.SummaryInvalidFilename)
	authDescriptor := synctypes.DescribeSummary(synctypes.SummaryAuthenticationRequired)
	sharedDescriptor := synctypes.DescribeSummary(synctypes.SummarySharedFolderWritesBlocked)

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
						IssueCount:       3,
						RemoteDrift:      2,
						Retrying:         1,
						LastError:        "sync: network timeout",
						StateStoreStatus: stateStoreStatusHealthy,
						ExamplesLimit:    defaultVisiblePaths,
						NextActions: []string{
							invalidDescriptor.Action,
							authDescriptor.Action,
							sharedDescriptor.Action,
						},
						IssueGroups: []failureGroupJSON{
							{
								SummaryKey: string(synctypes.SummaryInvalidFilename),
								IssueType:  string(synctypes.IssueInvalidFilename),
								Title:      invalidDescriptor.Title,
								Reason:     invalidDescriptor.Reason,
								Action:     invalidDescriptor.Action,
								Count:      1,
								Paths:      []string{"/invalid:name.txt"},
							},
							{
								SummaryKey: string(synctypes.SummaryAuthenticationRequired),
								IssueType:  string(synctypes.IssueUnauthorized),
								Title:      authDescriptor.Title,
								Reason:     authDescriptor.Reason,
								Action:     authDescriptor.Action,
								Count:      1,
								ScopeKind:  "account",
								Scope:      "your OneDrive account authorization",
								Paths:      nil,
							},
							{
								SummaryKey: string(synctypes.SummarySharedFolderWritesBlocked),
								IssueType:  string(synctypes.IssueSharedFolderBlocked),
								Title:      sharedDescriptor.Title,
								Reason:     sharedDescriptor.Reason,
								Action:     sharedDescriptor.Action,
								Count:      1,
								ScopeKind:  "shortcut",
								Scope:      "Shared/Docs",
								Paths:      []string{"Shared/Docs/report.docx"},
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
			AuthReason: authReasonInvalidSavedLogin,
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
