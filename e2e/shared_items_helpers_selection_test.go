package e2e

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sharedFileCandidate struct {
	fixture        resolvedSharedFileFixture
	listingMatched bool
}

func selectSharedFileFixture(candidates []sharedFileCandidate) (resolvedSharedFileFixture, error) {
	if len(candidates) == 0 {
		return resolvedSharedFileFixture{}, fmt.Errorf(
			"shared-file fixture did not resolve for any configured recipient account",
		)
	}

	if len(candidates) == 1 {
		return candidates[0].fixture, nil
	}

	var listingMatched []sharedFileCandidate
	for i := range candidates {
		if candidates[i].listingMatched {
			listingMatched = append(listingMatched, candidates[i])
		}
	}

	if len(listingMatched) == 1 {
		return listingMatched[0].fixture, nil
	}

	if !sharedFileCandidatesShareRemoteIdentity(candidates) {
		return resolvedSharedFileFixture{}, fmt.Errorf(
			"shared-file fixture resolved to multiple distinct configured recipient accounts (%d matches)",
			len(candidates),
		)
	}

	if len(listingMatched) > 0 {
		return listingMatched[0].fixture, nil
	}

	return candidates[0].fixture, nil
}

func sharedFileCandidatesShareRemoteIdentity(candidates []sharedFileCandidate) bool {
	if len(candidates) == 0 {
		return false
	}

	expectedDriveID := candidates[0].fixture.RawStat.RemoteDriveID
	expectedItemID := candidates[0].fixture.RawStat.RemoteItemID
	if expectedDriveID == "" || expectedItemID == "" {
		return false
	}

	for i := 1; i < len(candidates); i++ {
		if candidates[i].fixture.RawStat.RemoteDriveID != expectedDriveID ||
			candidates[i].fixture.RawStat.RemoteItemID != expectedItemID {
			return false
		}
	}

	return true
}

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
