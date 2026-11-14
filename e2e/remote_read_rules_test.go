package e2e

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/testutil"
)

// This file has no build tags, so its tests run in the untagged quick lane
// without an account. It decides what a live remote read does with one
// attempt, and what a timed-out poll prints; the live reads in
// remote_read_helpers_test.go run these decisions.

type remoteReadRecoveryAction uint8

const (
	remoteReadNoRecovery remoteReadRecoveryAction = iota
	remoteReadRefreshSharedRootListing
)

// errSharedRootListingLagging marks a read on a shared drive that failed while
// the mount root's listing lagged, when the listing did not answer before the
// read's deadline. Such a failure says nothing about the path read, so no
// poll accepts it.
var errSharedRootListingLagging = errors.New("shared-root listing did not answer before the deadline")

func isSharedCanonicalDriveID(driveID string) bool {
	canonicalID, err := driveid.NewCanonicalID(driveID)
	if err != nil {
		return false
	}

	return canonicalID.IsShared()
}

func classifyRemoteReadRecoveryAction(driveID string, stderr string) remoteReadRecoveryAction {
	if isSharedCanonicalDriveID(driveID) &&
		strings.Contains(stderr, "resolve item path") &&
		strings.Contains(stderr, "list children for segment") &&
		strings.Contains(stderr, "graph: HTTP 404") {
		return remoteReadRefreshSharedRootListing
	}

	return remoteReadNoRecovery
}

func isSharedRootListingProbe(args []string) bool {
	return len(args) == 2 && args[0] == "ls" && args[1] == "/"
}

// remoteReadNextStep is what a remote read does with the attempt it just ran.
type remoteReadNextStep uint8

const (
	// remoteReadOfferAttempt hands the attempt to the poll as it is.
	remoteReadOfferAttempt remoteReadNextStep = iota
	// remoteReadRetryAfterListing lists the mount root and runs the read
	// again once the listing answers.
	remoteReadRetryAfterListing
	// remoteReadMarkListingLagging hands the attempt on marked with
	// errSharedRootListingLagging: no time is left for the listing to
	// answer, so the failure says nothing about the path read.
	remoteReadMarkListingLagging
)

// decideRemoteReadNextStep decides what a read on driveID does after an
// attempt of args ended with err and stderr, with remaining left before the
// read's deadline. A read on a shared drive that failed because the mount
// root's listing lagged recovers through the listing while time is left, and
// is marked lagging when none is, so the last attempt follows the same rule
// as every other.
func decideRemoteReadNextStep(
	driveID string,
	args []string,
	stderr string,
	err error,
	remaining time.Duration,
) remoteReadNextStep {
	switch {
	case err == nil, isSharedRootListingProbe(args):
		return remoteReadOfferAttempt
	case classifyRemoteReadRecoveryAction(driveID, stderr) != remoteReadRefreshSharedRootListing:
		return remoteReadOfferAttempt
	case remaining <= 0:
		return remoteReadMarkListingLagging
	default:
		return remoteReadRetryAfterListing
	}
}

// markSharedRootListingLagging marks a read's failure as one that says
// nothing about the path read.
func markSharedRootListingLagging(err error) error {
	return fmt.Errorf("%w: %w", errSharedRootListingLagging, err)
}

// remotePollAccepts reports whether a poll takes one attempt: never one
// marked with errSharedRootListingLagging, whatever ready would say of it,
// and otherwise what ready says.
func remotePollAccepts(ready func(stdout, stderr string, err error) bool, stdout, stderr string, err error) bool {
	return !errors.Is(err, errSharedRootListingLagging) && ready(stdout, stderr, err)
}

// remotePollTimeoutMessage is the failure message of a remote poll that timed
// out: what it waited for, through which command, and the last attempt's
// output, with every configured drive and its account named by role, because
// the repository's CI logs are public.
func remotePollTimeoutMessage(
	roles testutil.DriveRoles,
	timeout time.Duration,
	description string,
	args []string,
	stdout string,
	stderr string,
) string {
	return roles.Redact(fmt.Sprintf(
		"after %v waiting for %s via %v\nlast stdout: %s\nlast stderr: %s",
		timeout, description, args, stdout, stderr,
	))
}

// sharedRootRoute404 is the error of a read on a shared drive whose mount
// root's listing lags: a 404 that says nothing about the path read.
const sharedRootRoute404 = `Error: resolving "/folder/file.txt": resolve item path "folder/file.txt": ` +
	`list children for segment "folder": graph: HTTP 404 (request-id: req-123): The resource could not be found.`

func TestClassifyRemoteReadRecoveryAction(t *testing.T) {
	t.Parallel()

	sharedCID, err := driveid.ConstructShared("user@example.com", "drive-id", "item-id")
	require.NoError(t, err)

	personalCID, err := driveid.NewCanonicalID("personal:user@example.com")
	require.NoError(t, err)

	require.Equal(t, remoteReadRefreshSharedRootListing, classifyRemoteReadRecoveryAction(sharedCID.String(), sharedRootRoute404))
	require.Equal(t, remoteReadNoRecovery, classifyRemoteReadRecoveryAction(personalCID.String(), sharedRootRoute404))
	require.Equal(t, remoteReadNoRecovery, classifyRemoteReadRecoveryAction(sharedCID.String(), `Error: creating folder "data": graph: HTTP 403: Access denied`))
	require.Equal(t, remoteReadNoRecovery, classifyRemoteReadRecoveryAction("not-a-canonical-id", sharedRootRoute404))
}

// A read that failed while the shared root's listing lagged recovers through
// the listing while time is left; with none left it is marked lagging, as
// every earlier attempt whose listing did not answer is, and never offered.
func TestDecideRemoteReadNextStep(t *testing.T) {
	t.Parallel()

	sharedCID, err := driveid.ConstructShared("alice@example.com", "fake-drive", "fake-item")
	require.NoError(t, err)
	shared := sharedCID.String()
	read := []string{"stat", "/folder/file.txt"}

	tests := []struct {
		name      string
		args      []string
		stderr    string
		err       error
		remaining time.Duration
		want      remoteReadNextStep
	}{
		{name: "a read that succeeded", args: read, want: remoteReadOfferAttempt, remaining: time.Minute},
		{name: "a read that succeeded and printed a lagging listing", args: read, stderr: sharedRootRoute404, remaining: time.Minute, want: remoteReadOfferAttempt},
		{name: "a read that succeeded with no time left", args: read, remaining: 0, want: remoteReadOfferAttempt},
		{name: "the root listing probe itself", args: []string{"ls", "/"}, stderr: sharedRootRoute404, err: assert.AnError, remaining: time.Minute, want: remoteReadOfferAttempt},
		{name: "a failure the listing does not explain", args: read, stderr: "graph: HTTP 403: Access denied", err: assert.AnError, remaining: time.Minute, want: remoteReadOfferAttempt},
		{name: "a lagging listing with time left", args: read, stderr: sharedRootRoute404, err: assert.AnError, remaining: time.Minute, want: remoteReadRetryAfterListing},
		{name: "a lagging listing at the deadline", args: read, stderr: sharedRootRoute404, err: assert.AnError, remaining: 0, want: remoteReadMarkListingLagging},
		{name: "a lagging listing past the deadline", args: read, stderr: sharedRootRoute404, err: assert.AnError, remaining: -time.Second, want: remoteReadMarkListingLagging},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, decideRemoteReadNextStep(shared, tt.args, tt.stderr, tt.err, tt.remaining))
		})
	}
}

// A predicate that reads "not found" as "gone" must never see a lagging
// listing's 404: the poll refuses an attempt marked lagging whatever the
// predicate would say, and offers every other attempt to it.
func TestRemotePollAcceptsNoAttemptMarkedLagging(t *testing.T) {
	t.Parallel()

	readsNotFoundAsGone := func(_, stderr string, err error) bool {
		return err == nil || strings.Contains(strings.ToLower(stderr), "could not be found")
	}

	assert.False(t, remotePollAccepts(readsNotFoundAsGone, "", sharedRootRoute404, markSharedRootListingLagging(assert.AnError)),
		"a lagging listing's 404 says nothing about the path")
	assert.True(t, remotePollAccepts(readsNotFoundAsGone, "", sharedRootRoute404, assert.AnError),
		"a 404 the listing recovery let through is the predicate's to judge")
	assert.True(t, remotePollAccepts(readsNotFoundAsGone, "a.txt\n", "", nil))
	assert.False(t, remotePollAccepts(readsNotFoundAsGone, "", "graph: HTTP 503", assert.AnError))
}

// A timed-out poll prints its last output for diagnosis, with each configured
// drive and its account named by role in every spelling a command or a Graph
// URL can print.
//
// Validates: R-6.10.27
func TestRemotePollTimeoutMessageNamesDrivesByRole(t *testing.T) {
	t.Parallel()

	const account = "alice@example.com"
	canonicalID := "personal:" + account
	roles := testutil.NewDriveRoles(canonicalID)

	message := remotePollTimeoutMessage(
		roles,
		time.Minute,
		"status reports "+canonicalID+" no longer paused",
		[]string{"--drive", canonicalID, "stat", "/a.txt"},
		`{"account_email":"`+account+`"}`,
		"GET https://graph.microsoft.com/v1.0/users/alice%40example.com/drive\nstate_personal_alice@example.com.db",
	)

	assert.NotContains(t, message, account)
	assert.NotContains(t, strings.ToLower(message), "alice%40example.com")
	assert.Contains(t, message, "status reports drive-1 no longer paused")
	assert.Contains(t, message, `{"account_email":"drive-1-account"}`)
	assert.Contains(t, message, "users/drive-1-account/drive")
	assert.Contains(t, message, "state_drive-1.db")
	assert.Contains(t, message, "via [--drive drive-1 stat /a.txt]")
}
