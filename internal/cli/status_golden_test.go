package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func goldenStatusAccounts() []statusAccount {
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
						Issues:           3,
						IssueGroups: []statusIssueGroup{
							{
								SummaryKey: string(synctypes.SummaryInvalidFilename),
								Title:      "INVALID FILENAME",
								Count:      1,
								ScopeKind:  "file",
							},
							{
								SummaryKey: string(synctypes.SummaryAuthenticationRequired),
								Title:      "AUTHENTICATION REQUIRED",
								Count:      1,
								ScopeKind:  "account",
								Scope:      "your OneDrive account authorization",
							},
							{
								SummaryKey: string(synctypes.SummarySharedFolderWritesBlocked),
								Title:      "SHARED FOLDER WRITES BLOCKED",
								Count:      1,
								ScopeKind:  "shortcut",
								Scope:      "Shared/Docs",
							},
						},
						PendingSync: 2,
						Retrying:    1,
						LastError:   "sync: network timeout",
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
	require.NoError(t, printStatusText(&buf, goldenStatusAccounts()))
	assertGoldenFile(t, "status_text.golden", buf.Bytes())
}

// Validates: R-2.14.3
func TestStatusOutputGoldenJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printStatusJSON(&buf, goldenStatusAccounts()))
	assertGoldenFile(t, "status_json.golden", buf.Bytes())
}
