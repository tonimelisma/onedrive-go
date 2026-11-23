package e2e

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type liveProviderSkipReason string

const (
	liveProviderSkipNone                    liveProviderSkipReason = ""
	liveProviderSkipDriveListSharedOmitted  liveProviderSkipReason = "drive_list_shared_selector_omitted"
	liveProviderSkipSharedFolderNameOmitted liveProviderSkipReason = "shared_folder_name_omitted"
	liveProviderSkipSharedDiscoveryGateway  liveProviderSkipReason = "shared_discovery_gateway"
	liveProviderSkipShortcutRootOmitted     liveProviderSkipReason = "shortcut_root_placeholder_omitted"
)

type liveProviderSkipDecision struct {
	Skippable bool
	Reason    liveProviderSkipReason
}

func driveListAvailableContainsCanonicalID(parsed driveListE2EOutput, canonicalID string) bool {
	for i := range parsed.Available {
		if parsed.Available[i].CanonicalID == canonicalID {
			return true
		}
	}

	return false
}

func TestDriveListAvailableContainsCanonicalID(t *testing.T) {
	t.Parallel()

	parsed := driveListE2EOutput{
		Available: []struct {
			CanonicalID string `json:"canonical_id"`
		}{
			{CanonicalID: "shared:user@example.com:drive-a:item-1"},
			{CanonicalID: "shared:user@example.com:drive-b:item-2"},
		},
	}

	require.True(t, driveListAvailableContainsCanonicalID(parsed, "shared:user@example.com:drive-a:item-1"))
	require.False(t, driveListAvailableContainsCanonicalID(parsed, "shared:user@example.com:drive-c:item-3"))
}

func isSharedFolderNameDiscoveryOmission(stderr string) bool {
	return strings.Contains(stderr, "no shared folders matching") &&
		strings.Contains(stderr, "Graph shared discovery")
}

func TestIsSharedFolderNameDiscoveryOmission(t *testing.T) {
	t.Parallel()

	assert.True(t, isSharedFolderNameDiscoveryOmission(
		`no shared folders matching "Project" found - Graph shared discovery also checks external shares`))
	assert.False(t, isSharedFolderNameDiscoveryOmission(`graph: HTTP 503 Service Unavailable`))
	assert.False(t, isSharedFolderNameDiscoveryOmission(`no shared folders matching "Project" found`))
}

func sharedFolderFixtureVisibleInListing(listing sharedListE2EOutput, fixture *resolvedSharedFolderFixture) bool {
	for i := range listing.Items {
		item := listing.Items[i]
		if item.Type != sharedItemTypeFolder {
			continue
		}
		if item.Name != fixture.FolderItem.Name {
			continue
		}
		if item.RemoteDriveID != fixture.FolderItem.RemoteDriveID {
			continue
		}
		if item.RemoteItemID != fixture.FolderItem.RemoteItemID {
			continue
		}

		return true
	}

	return false
}

func TestSharedFolderFixtureVisibleInListingRequiresNameAndRemoteIdentity(t *testing.T) {
	t.Parallel()

	fixture := &resolvedSharedFolderFixture{
		FolderItem: sharedItemE2E{
			Name:          "Project",
			RemoteDriveID: "drive",
			RemoteItemID:  "item",
		},
	}

	assert.True(t, sharedFolderFixtureVisibleInListing(sharedListE2EOutput{
		Items: []sharedItemE2E{{
			Type:          sharedItemTypeFolder,
			Name:          "Project",
			RemoteDriveID: "drive",
			RemoteItemID:  "item",
		}},
	}, fixture))
	assert.False(t, sharedFolderFixtureVisibleInListing(sharedListE2EOutput{
		Items: []sharedItemE2E{{
			Type:          sharedItemTypeFolder,
			Name:          "Other",
			RemoteDriveID: "drive",
			RemoteItemID:  "item",
		}},
	}, fixture))
	assert.False(t, sharedFolderFixtureVisibleInListing(sharedListE2EOutput{
		Items: []sharedItemE2E{{
			Type:          sharedItemTypeFolder,
			Name:          "Project",
			RemoteDriveID: "other-drive",
			RemoteItemID:  "item",
		}},
	}, fixture))
	assert.False(t, sharedFolderFixtureVisibleInListing(sharedListE2EOutput{
		Items: []sharedItemE2E{{
			Type:          sharedItemTypeFolder,
			Name:          "Project",
			RemoteDriveID: "drive",
			RemoteItemID:  "other-item",
		}},
	}, fixture))
	assert.False(t, sharedFolderFixtureVisibleInListing(sharedListE2EOutput{
		Items: []sharedItemE2E{{
			Type:          sharedItemTypeFile,
			Name:          "Project",
			RemoteDriveID: "drive",
			RemoteItemID:  "item",
		}},
	}, fixture))
}

func TestSharedFolderNameProviderOmittedFixtureRequiresIndependentProviderSignal(t *testing.T) {
	t.Parallel()

	noMatch := `no shared folders matching "Project" found — Graph shared discovery also checks external shares`

	decision := classifySharedFolderNameProviderSkip(noMatch, false, nil, "")
	assert.True(t, decision.Skippable)
	assert.Equal(t, liveProviderSkipSharedFolderNameOmitted, decision.Reason)

	decision = classifySharedFolderNameProviderSkip(noMatch, false, errors.New("shared failed"), "graph: HTTP 503 Service Unavailable")
	assert.True(t, decision.Skippable)
	assert.Equal(t, liveProviderSkipSharedDiscoveryGateway, decision.Reason)

	assert.False(t, classifySharedFolderNameProviderSkip(noMatch, true, nil, "").Skippable)
	assert.False(t, classifySharedFolderNameProviderSkip(noMatch, false, errors.New("decode failed"), "not json").Skippable)
	assert.False(t, classifySharedFolderNameProviderSkip("graph: HTTP 503 Service Unavailable", false, nil, "").Skippable)
}

func classifySharedFolderNameProviderSkip(
	driveAddStderr string,
	sharedVisible bool,
	sharedErr error,
	sharedStderr string,
) liveProviderSkipDecision {
	if !isSharedFolderNameDiscoveryOmission(driveAddStderr) {
		return liveProviderSkipDecision{Reason: liveProviderSkipNone}
	}
	if sharedErr != nil {
		if isRetryableGraphGatewayFailure(sharedStderr) {
			return liveProviderSkipDecision{Skippable: true, Reason: liveProviderSkipSharedDiscoveryGateway}
		}
		return liveProviderSkipDecision{Reason: liveProviderSkipNone}
	}

	if sharedVisible {
		return liveProviderSkipDecision{Reason: liveProviderSkipNone}
	}
	return liveProviderSkipDecision{Skippable: true, Reason: liveProviderSkipSharedFolderNameOmitted}
}

// shortcutFixtureProviderOmissionProofDelay is the shorter window after which a
// decoded parent-root listing plus independently reachable shared target is
// enough evidence to classify a missing shortcut placeholder as provider
// omission instead of spending the full fixture propagation budget.
const shortcutFixtureProviderOmissionProofDelay = 30 * time.Second

func shortcutRootPlaceholderProviderProofElapsed(startedAt, now time.Time) bool {
	return !now.Before(startedAt.Add(shortcutFixtureProviderOmissionProofDelay))
}

type shortcutRootPlaceholderPollState struct {
	RootListingDecoded bool
	RootNames          []string
	LastStdout         string
	LastErr            error
}

func canSkipMissingShortcutRootPlaceholder(
	state shortcutRootPlaceholderPollState,
	fixture *resolvedShortcutFixture,
) bool {
	return classifyShortcutRootPlaceholderProviderSkip(state, fixture).Skippable
}

func classifyShortcutRootPlaceholderProviderSkip(
	state shortcutRootPlaceholderPollState,
	fixture *resolvedShortcutFixture,
) liveProviderSkipDecision {
	if !state.RootListingDecoded || state.LastErr != nil || fixture.SharedItem.Selector == "" {
		return liveProviderSkipDecision{Reason: liveProviderSkipNone}
	}
	return liveProviderSkipDecision{Skippable: true, Reason: liveProviderSkipShortcutRootOmitted}
}

func TestCanSkipMissingShortcutRootPlaceholderRequiresDecodedListingAndSharedTarget(t *testing.T) {
	t.Parallel()

	fixture := &resolvedShortcutFixture{
		SharedItem: sharedItemE2E{
			Selector: "shared:user@example.com:drive:item",
		},
	}

	assert.True(t, canSkipMissingShortcutRootPlaceholder(
		shortcutRootPlaceholderPollState{RootListingDecoded: true},
		fixture,
	))
	assert.False(t, canSkipMissingShortcutRootPlaceholder(
		shortcutRootPlaceholderPollState{
			RootListingDecoded: true,
			LastErr:            errors.New("graph unavailable"),
		},
		fixture,
	))
	assert.False(t, canSkipMissingShortcutRootPlaceholder(
		shortcutRootPlaceholderPollState{RootListingDecoded: false},
		fixture,
	))
	assert.False(t, canSkipMissingShortcutRootPlaceholder(
		shortcutRootPlaceholderPollState{RootListingDecoded: true},
		&resolvedShortcutFixture{},
	))
}

func TestShortcutRootPlaceholderProviderProofElapsed(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)

	assert.False(t, shortcutRootPlaceholderProviderProofElapsed(
		startedAt,
		startedAt.Add(shortcutFixtureProviderOmissionProofDelay-time.Nanosecond),
	))
	assert.True(t, shortcutRootPlaceholderProviderProofElapsed(
		startedAt,
		startedAt.Add(shortcutFixtureProviderOmissionProofDelay),
	))
}
