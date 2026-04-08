//go:build e2e

package e2e

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelectSharedFileFixture_PrefersUniqueListingMatch(t *testing.T) {
	t.Parallel()

	fixture, err := selectSharedFileFixture([]sharedFileCandidate{
		{
			fixture: resolvedSharedFileFixture{
				RecipientDriveID: "personal:first@example.com",
				RecipientEmail:   "first@example.com",
				RawStat: sharedStatE2EOutput{
					RemoteDriveID: "drive-1",
					RemoteItemID:  "item-1",
				},
			},
			listingMatched: false,
		},
		{
			fixture: resolvedSharedFileFixture{
				RecipientDriveID: "personal:second@example.com",
				RecipientEmail:   "second@example.com",
				RawStat: sharedStatE2EOutput{
					RemoteDriveID: "drive-2",
					RemoteItemID:  "item-2",
				},
			},
			listingMatched: true,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "second@example.com", fixture.RecipientEmail)
}

func TestSelectSharedFileFixture_AcceptsMultipleRecipientsForSameRemoteIdentity(t *testing.T) {
	t.Parallel()

	fixture, err := selectSharedFileFixture([]sharedFileCandidate{
		{
			fixture: resolvedSharedFileFixture{
				RecipientDriveID: "personal:first@example.com",
				RecipientEmail:   "first@example.com",
				RawStat: sharedStatE2EOutput{
					RemoteDriveID: "shared-drive",
					RemoteItemID:  "shared-item",
				},
			},
			listingMatched: false,
		},
		{
			fixture: resolvedSharedFileFixture{
				RecipientDriveID: "personal:second@example.com",
				RecipientEmail:   "second@example.com",
				RawStat: sharedStatE2EOutput{
					RemoteDriveID: "shared-drive",
					RemoteItemID:  "shared-item",
				},
			},
			listingMatched: true,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "second@example.com", fixture.RecipientEmail)
}

func TestSelectSharedFileFixture_RejectsDistinctRemoteIdentities(t *testing.T) {
	t.Parallel()

	_, err := selectSharedFileFixture([]sharedFileCandidate{
		{
			fixture: resolvedSharedFileFixture{
				RecipientDriveID: "personal:first@example.com",
				RecipientEmail:   "first@example.com",
				RawStat: sharedStatE2EOutput{
					RemoteDriveID: "drive-1",
					RemoteItemID:  "item-1",
				},
			},
			listingMatched: true,
		},
		{
			fixture: resolvedSharedFileFixture{
				RecipientDriveID: "personal:second@example.com",
				RecipientEmail:   "second@example.com",
				RawStat: sharedStatE2EOutput{
					RemoteDriveID: "drive-2",
					RemoteItemID:  "item-2",
				},
			},
			listingMatched: true,
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple distinct configured recipient accounts")
}
