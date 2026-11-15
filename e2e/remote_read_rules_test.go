package e2e

import (
	"encoding/json"
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
// attempt, which attempts the suite's waits accept, and what a timed-out poll
// prints; the live reads in remote_read_helpers_test.go run these decisions.

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

// The sentence the CLI prints for an item OneDrive does not have
// (remoteFailureText in internal/cli/error_presentation.go): "Error: <path>
// was not found on OneDrive." and, for a path with a parent, a hint to list
// the parent.
const (
	cliErrorPrefix            = "Error: "
	remoteItemMissingSentence = " was not found on OneDrive."
	listParentHintPrefix      = " Run 'onedrive-go ls "
	listParentHintSuffix      = "' to see what is there."
)

// cliReportsRemoteItemMissing reports whether a failed command's stderr holds
// the CLI's own sentence for an item OneDrive does not have, on the line it
// prints as its error. Under --debug, stderr also holds the debug log, whose
// lines can say "not found" of a request the command retried or recovered
// from before it failed for another reason.
func cliReportsRemoteItemMissing(stderr string) bool {
	for line := range strings.Lines(stderr) {
		message, isError := strings.CutPrefix(strings.TrimRight(line, "\r\n"), cliErrorPrefix)
		if !isError {
			continue
		}

		_, rest, isMissing := strings.Cut(message, remoteItemMissingSentence)
		if isMissing && (rest == "" ||
			strings.HasPrefix(rest, listParentHintPrefix) && strings.HasSuffix(rest, listParentHintSuffix)) {
			return true
		}
	}

	return false
}

// The CLI's sentences for a missing item, as `rm`, `stat` and `ls` print them
// under --debug: the debug log, the error, then its details.
const (
	missingFolderStderr = `time=2026-11-14T10:00:00Z level=DEBUG msg="graph response" status=404` + "\n" +
		`Error: /e2e-folder was not found on OneDrive. Run 'onedrive-go ls /' to see what is there.` + "\n" +
		`Details: resolving "/e2e-folder": resolve item path "e2e-folder": graph: HTTP 404: itemNotFound` + "\n"
	missingFileStderr = `Error: /e2e-folder/a.txt was not found on OneDrive. ` +
		`Run 'onedrive-go ls /e2e-folder' to see what is there.` + "\n"
)

// Validates: R-6.10.17
func TestCLIReportsRemoteItemMissing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stderr string
		want   bool
	}{
		{name: "a folder rm -r or ls did not find", stderr: missingFolderStderr, want: true},
		{name: "a file stat did not find", stderr: missingFileStderr, want: true},
		{name: "the drive root, which has no parent to list", stderr: "Error: / was not found on OneDrive.\n", want: true},
		{name: "a failure that recorded no path", stderr: "Error: the item was not found on OneDrive.", want: true},
		{
			name: "a debug line that holds the words, before another failure",
			stderr: `time=2026-11-14T10:00:00Z level=DEBUG msg="retrying" body="itemNotFound: The resource could not be found."` + "\n" +
				`time=2026-11-14T10:00:01Z level=DEBUG msg="lookup" error="parent not found"` + "\n" +
				"Error: OneDrive had a temporary server problem. Try again in a few minutes.\n",
			want: false,
		},
		{
			name:   "the sentence in a debug line, not as the error",
			stderr: `level=DEBUG msg="/a.txt was not found on OneDrive."` + "\nError: OneDrive denied access to /a.txt. Check that this account can open it.\n",
			want:   false,
		},
		{name: "an error that goes on after the sentence", stderr: "Error: /a.txt was not found on OneDrive. And more.\n", want: false},
		{name: "no output", stderr: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, cliReportsRemoteItemMissing(tt.stderr))
		})
	}
}

type fixtureSeedVisibilityOutcome uint8

const (
	fixtureSeedVisibilityKeepPolling fixtureSeedVisibilityOutcome = iota
	fixtureSeedVisibilityExactSuccess
	fixtureSeedVisibilitySoftenedByParentListing
)

// classifyFixtureSeedVisibilityAttempt keeps the fixture-readiness contract
// pure: exact stat success wins immediately, otherwise a visible parent listing
// softens the lag into the documented post-mutation destination recurrence.
func classifyFixtureSeedVisibilityAttempt(
	targetBase string,
	statErr error,
	parentListStdout string,
	parentListErr error,
) fixtureSeedVisibilityOutcome {
	switch {
	case statErr == nil:
		return fixtureSeedVisibilityExactSuccess
	case parentListErr == nil && strings.Contains(parentListStdout, targetBase):
		return fixtureSeedVisibilitySoftenedByParentListing
	default:
		return fixtureSeedVisibilityKeepPolling
	}
}

func TestClassifyFixtureSeedVisibilityAttempt(t *testing.T) {
	t.Parallel()

	assert.Equal(t, fixtureSeedVisibilityExactSuccess, classifyFixtureSeedVisibilityAttempt(
		"test.txt",
		nil,
		"",
		nil,
	))
	assert.Equal(t, fixtureSeedVisibilitySoftenedByParentListing, classifyFixtureSeedVisibilityAttempt(
		"test.txt",
		assert.AnError,
		"test.txt\nother.txt\n",
		nil,
	))
	assert.Equal(t, fixtureSeedVisibilityKeepPolling, classifyFixtureSeedVisibilityAttempt(
		"test.txt",
		assert.AnError,
		"other.txt\n",
		nil,
	))
	assert.Equal(t, fixtureSeedVisibilityKeepPolling, classifyFixtureSeedVisibilityAttempt(
		"test.txt",
		assert.AnError,
		"",
		assert.AnError,
	))
}

// remoteStatSizeReady reports whether one `stat --json` attempt answered with
// the item at exactly size bytes.
func remoteStatSizeReady(stdout string, err error, size int64) bool {
	if err != nil {
		return false
	}

	var stat struct {
		Size int64 `json:"size"`
	}

	return json.Unmarshal([]byte(stdout), &stat) == nil && stat.Size == size
}

// The empty item a create leaves before its content is the case the size wait
// exists to reject (LI-20261110-01).
func TestRemoteStatSizeReady(t *testing.T) {
	t.Parallel()

	const contentSize = 21

	assert.True(t, remoteStatSizeReady(`{"name":"a.txt","size":21}`, nil, contentSize))
	assert.False(t, remoteStatSizeReady(`{"name":"a.txt","size":0}`, nil, contentSize), "the empty item before its content")
	assert.False(t, remoteStatSizeReady(`{"name":"a.txt","size":21}`, assert.AnError, contentSize), "a failed stat")
	assert.False(t, remoteStatSizeReady("Size: 21 B (21 bytes)\n", nil, contentSize), "human output instead of JSON")
	assert.False(t, remoteStatSizeReady("", nil, contentSize), "no output")
}

// remoteStatCreatedAt reads created_at from one `stat --json` answer: when
// OneDrive created the item (createdDateTime), a time no upload sends.
func remoteStatCreatedAt(stdout string) (time.Time, error) {
	var stat struct {
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal([]byte(stdout), &stat); err != nil {
		return time.Time{}, fmt.Errorf("decode stat --json: %w", err)
	}

	createdAt, err := time.Parse(time.RFC3339, stat.CreatedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse created_at %q: %w", stat.CreatedAt, err)
	}

	return createdAt, nil
}

// Validates: R-2.8.12
func TestRemoteStatCreatedAt(t *testing.T) {
	t.Parallel()

	createdAt, err := remoteStatCreatedAt(`{"name":"a.txt","size":21,"created_at":"2026-11-14T10:00:05Z"}`)
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, time.November, 14, 10, 0, 5, 0, time.UTC), createdAt)

	_, err = remoteStatCreatedAt(`{"name":"a.txt","size":21}`)
	require.ErrorContains(t, err, "parse created_at", "an answer without created_at")

	_, err = remoteStatCreatedAt("Created:  2026-11-14 10:00:05\n")
	require.ErrorContains(t, err, "decode stat --json", "human output instead of JSON")
}

// pauseCommandEnd reads paused_until from one `pause --json` answer: the end
// the command stored for the pause, which is the time the daemon resumes at.
func pauseCommandEnd(stdout string) (time.Time, error) {
	var pause struct {
		PausedUntil string `json:"paused_until"`
	}
	if err := json.Unmarshal([]byte(stdout), &pause); err != nil {
		return time.Time{}, fmt.Errorf("decode pause --json: %w", err)
	}

	pausedUntil, err := time.Parse(time.RFC3339, pause.PausedUntil)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse paused_until %q: %w", pause.PausedUntil, err)
	}

	return pausedUntil, nil
}

// Validates: R-2.8.12
func TestPauseCommandEnd(t *testing.T) {
	t.Parallel()

	pausedUntil, err := pauseCommandEnd(`{"drive":"personal:someone@example.com","paused":true,"paused_until":"2026-11-14T10:00:30Z"}`)
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, time.November, 14, 10, 0, 30, 0, time.UTC), pausedUntil)

	_, err = pauseCommandEnd(`{"drive":"personal:someone@example.com","paused":true}`)
	require.ErrorContains(t, err, "parse paused_until", "a pause without an end")

	_, err = pauseCommandEnd("Paused the drive until Sat Nov 14 10:00.\n")
	require.ErrorContains(t, err, "decode pause --json", "human output instead of JSON")
}

// deleteDisappearanceReady reports whether one listing attempt shows the
// unexpected name gone: a listing without it, or the CLI saying the listed
// folder itself is missing.
func deleteDisappearanceReady(stdout string, stderr string, err error, unexpected string) bool {
	switch {
	case err == nil:
		return !strings.Contains(stdout, unexpected)
	case cliReportsRemoteItemMissing(stderr):
		return true
	default:
		return false
	}
}

func TestDeleteDisappearanceReady(t *testing.T) {
	t.Parallel()

	assert.True(t, deleteDisappearanceReady("other.txt\n", "", nil, "target.txt"))
	assert.False(t, deleteDisappearanceReady("target.txt\n", "", nil, "target.txt"))
	assert.True(t, deleteDisappearanceReady("", missingFolderStderr, assert.AnError, "target.txt"), "the folder is gone")
	assert.False(t, deleteDisappearanceReady("", "transport timeout", assert.AnError, "target.txt"))
	assert.False(t, deleteDisappearanceReady("", `level=DEBUG body="The resource could not be found."`+
		"\nError: OneDrive had a temporary server problem.", assert.AnError, "target.txt"), "a failure whose debug log holds a 404")
}

// remoteListingEmptyOrGone reports whether one `ls --json` attempt shows
// nothing left under the folder it lists: an empty listing, or the CLI saying
// the folder itself is missing.
func remoteListingEmptyOrGone(stdout, stderr string, err error) bool {
	if err != nil {
		return cliReportsRemoteItemMissing(stderr)
	}

	var items []json.RawMessage

	return json.Unmarshal([]byte(stdout), &items) == nil && len(items) == 0
}

// Validates: R-6.10.17
func TestRemoteListingEmptyOrGone(t *testing.T) {
	t.Parallel()

	assert.True(t, remoteListingEmptyOrGone("[]\n", "", nil), "an empty listing")
	assert.True(t, remoteListingEmptyOrGone("", missingFolderStderr, assert.AnError), "the folder is gone")
	assert.False(t, remoteListingEmptyOrGone(`[{"name":"f00.txt"}]`, "", nil), "a file is left")
	assert.False(t, remoteListingEmptyOrGone("", "", nil), "no output")
	assert.False(t, remoteListingEmptyOrGone("", "Error: OneDrive had a temporary server problem.", assert.AnError),
		"a failed listing says nothing about what is there")
}
