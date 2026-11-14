//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitForSharedRootListingVisible lists the mount root of driveID until the
// listing answers, and fails the test when remoteWritePropagationTimeout
// passes first.
func waitForSharedRootListingVisible(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
) {
	t.Helper()

	pollRemoteEventually(
		t,
		cfgPath,
		env,
		driveID,
		remoteWritePropagationTimeout,
		timingKindRemoteWriteVisibility,
		sharedRootListingDescription(driveID),
		remoteReadSucceeded,
		"ls",
		"/",
	)
}

// sharedRootListingAnswersWithin lists the mount root of driveID until the
// listing answers or timeout passes, and reports whether it answered. It fails
// nothing itself, so a read that recovers through it keeps its own deadline
// and its own failure message.
func sharedRootListingAnswersWithin(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	timeout time.Duration,
) bool {
	t.Helper()

	_, _, isAnswered := pollRemoteUntil(
		t,
		cfgPath,
		env,
		driveID,
		timeout,
		timingKindRemoteWriteVisibility,
		sharedRootListingDescription(driveID),
		remoteReadSucceeded,
		"ls",
		"/",
	)

	return isAnswered
}

func sharedRootListingDescription(driveID string) string {
	return "shared-root listing visibility for " + suiteDriveRoles.Role(driveID)
}

// remoteReadSucceeded accepts any read whose command succeeded.
func remoteReadSucceeded(_, _ string, err error) bool {
	return err == nil
}

// runRemoteReadAttemptWithRecovery runs one CLI read. A read on a shared drive
// that fails because the mount root's listing lags is run again once the
// listing answers (decideRemoteReadNextStep). It fails nothing itself: when
// the listing does not answer before deadline, the deadline has already
// passed, or the read run again meets a lagging listing once more, it returns
// the failed read with errSharedRootListingLagging in its error, and the
// caller goes on to its own deadline and message.
func runRemoteReadAttemptWithRecovery(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	deadline time.Time,
	args ...string,
) (string, string, error) {
	t.Helper()

	stdout, stderr, err := runCLICore(t, cfgPath, env, driveID, args...)
	remaining := time.Until(deadline)
	step := decideRemoteReadNextStep(driveID, args, stderr, err, remaining)
	if step == remoteReadOfferAttempt {
		return stdout, stderr, err
	}

	if step == remoteReadRetryAfterListing && sharedRootListingAnswersWithin(t, cfgPath, env, driveID, remaining) {
		// One recovery per attempt: the read run again is decided as a read
		// with no time left, so a listing that lags again marks it.
		stdout, stderr, err = runCLICore(t, cfgPath, env, driveID, args...)
		if decideRemoteReadNextStep(driveID, args, stderr, err, 0) == remoteReadOfferAttempt {
			return stdout, stderr, err
		}
	}

	return stdout, stderr, markSharedRootListingLagging(err)
}

// pollRemoteEventually runs a CLI read until ready accepts an attempt, and
// fails the test with the last attempt's output, every configured drive in it
// named by role, when timeout passes first.
func pollRemoteEventually(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	timeout time.Duration,
	eventKind string,
	description string,
	ready func(stdout, stderr string, err error) bool,
	args ...string,
) (string, string) {
	t.Helper()

	stdout, stderr, isReady := pollRemoteUntil(t, cfgPath, env, driveID, timeout, eventKind, description, ready, args...)
	if !isReady {
		require.Failf(t, "pollRemoteEventually: timed out", "%s",
			remotePollTimeoutMessage(suiteDriveRoles, timeout, description, args, stdout, stderr))
	}

	return stdout, stderr
}

// pollRemoteUntil runs a CLI read until ready accepts an attempt or timeout
// passes, and records the wait's timing either way. It reports whether ready
// accepted one, with the last attempt's output, and fails nothing itself, so
// a wait that knows more than the output can say what it last saw. An attempt
// whose shared-root recovery found no listing before the deadline is never
// offered to ready: its failure says nothing about the path read.
func pollRemoteUntil(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	timeout time.Duration,
	eventKind string,
	description string,
	ready func(stdout, stderr string, err error) bool,
	args ...string,
) (string, string, bool) {
	t.Helper()

	startedAt := time.Now()
	deadline := time.Now().Add(timeout)
	resolvedDriveID := resolveDriveSelection(env, driveID)

	for attempt := 0; ; attempt++ {
		stdout, stderr, err := runRemoteReadAttemptWithRecovery(
			t,
			cfgPath,
			env,
			resolvedDriveID,
			deadline,
			args...,
		)
		if remotePollAccepts(ready, stdout, stderr, err) {
			recordTimingEvent(
				t,
				eventKind,
				description,
				resolvedDriveID,
				args,
				timeout,
				time.Since(startedAt),
				attempt+1,
				timingOutcomeSuccess,
			)
			return stdout, stderr, true
		}

		if time.Now().After(deadline) {
			recordTimingEvent(
				t,
				eventKind,
				description,
				resolvedDriveID,
				args,
				timeout,
				time.Since(startedAt),
				attempt+1,
				timingOutcomeTimeout,
			)
			return stdout, stderr, false
		}

		sleepForLiveTestPropagation(pollBackoff(attempt))
	}
}

func waitForRemoteReadContains(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	expected string,
	timeout time.Duration,
	args ...string,
) (string, string) {
	t.Helper()

	return pollRemoteEventually(
		t,
		cfgPath,
		env,
		driveID,
		timeout,
		timingKindRemoteWriteVisibility,
		fmt.Sprintf("remote read contains %q", expected),
		func(stdout, _ string, err error) bool {
			return err == nil && strings.Contains(stdout, expected)
		},
		args...,
	)
}

// waitForRemoteStatSize is waitForRemoteStatSizeForDrive on the drive that env
// selects, or else on the suite's primary drive.
func waitForRemoteStatSize(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	remotePath string,
	size int64,
	timeout time.Duration,
) {
	t.Helper()

	waitForRemoteStatSizeForDrive(t, cfgPath, env, "", remotePath, size, timeout)
}

// waitForRemoteStatSizeForDrive waits until `stat --json` reports remotePath
// at size bytes on driveID. This is the wait for a new file a running daemon
// uploads: the file is created empty under its final name and filled by a
// second request, so its name is visible while it still has 0 bytes, and a
// test that stops the daemon on the name alone can stop it between the two
// requests and leave the empty item behind (LI-20261110-01). The size of a new
// file changes only when its content commits. An overwrite keeps the old
// content until the new commits, so a change to content of the same length
// needs waitForRemoteFileContent instead.
//
// It reads the JSON document rather than the human "N bytes" text, so the wait
// cannot break when stat's human formatting changes.
func waitForRemoteStatSizeForDrive(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	remotePath string,
	size int64,
	timeout time.Duration,
) {
	t.Helper()

	pollRemoteEventually(
		t,
		cfgPath,
		env,
		driveID,
		timeout,
		timingKindRemoteWriteVisibility,
		fmt.Sprintf("remote stat size %d for %s", size, remotePath),
		func(stdout, _ string, err error) bool {
			return remoteStatSizeReady(stdout, err, size)
		},
		"--json", "stat", remotePath,
	)
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

// waitForRemoteExactStatVisible is for tests whose contract is the exact path
// route itself. It should not be used for generic fixture seeding.
func waitForRemoteExactStatVisible(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	remotePath string,
) (string, string) {
	t.Helper()

	return pollRemoteEventually(
		t,
		cfgPath,
		env,
		driveID,
		remoteWritePropagationTimeout,
		timingKindRemoteWriteVisibility,
		fmt.Sprintf("remote exact-path visibility for %q", remotePath),
		func(_ string, _ string, err error) bool {
			return err == nil
		},
		"stat", remotePath,
	)
}

// waitForRemoteParentListingContains proves list-visible availability under a
// parent path without asserting exact-path convergence.
func waitForRemoteParentListingContains(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	parentPath string,
	expected string,
) (string, string) {
	t.Helper()

	return waitForRemoteReadContains(
		t,
		cfgPath,
		env,
		driveID,
		expected,
		remoteWritePropagationTimeout,
		"ls", parentPath,
	)
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

// waitForRemoteFixtureSeedVisible is the shared fixture-readiness contract for
// remote writes that are only setup for later assertions. It accepts either
// exact stat success or parent-list visibility so unrelated tests do not depend
// on one stricter read path winning a provider convergence race.
func waitForRemoteFixtureSeedVisible(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	remotePath string,
) {
	t.Helper()

	resolvedDriveID := resolveDriveSelection(env, driveID)
	cleanPath := path.Clean(remotePath)
	if cleanPath == "." || cleanPath == "/" || cleanPath == "" {
		return
	}

	base := path.Base(cleanPath)
	if base == "." || base == "/" || base == "" {
		return
	}

	parentPath := path.Dir(cleanPath)
	if parentPath == "." || parentPath == "" {
		parentPath = "/"
	}
	deadline := time.Now().Add(remoteWritePropagationTimeout)

	pollRemoteEventually(
		t,
		cfgPath,
		env,
		resolvedDriveID,
		remoteWritePropagationTimeout,
		timingKindRemoteWriteVisibility,
		fmt.Sprintf("remote fixture seed visibility for %q via exact stat or parent listing", cleanPath),
		func(_ string, statStderr string, statErr error) bool {
			if classifyFixtureSeedVisibilityAttempt(base, statErr, "", nil) == fixtureSeedVisibilityExactSuccess {
				return true
			}

			listStdout, _, listErr := runRemoteReadAttemptWithRecovery(
				t,
				cfgPath,
				env,
				resolvedDriveID,
				deadline,
				"ls",
				parentPath,
			)
			if classifyFixtureSeedVisibilityAttempt(base, statErr, listStdout, listErr) == fixtureSeedVisibilitySoftenedByParentListing {
				recordLiveProviderRecurrenceEvent(
					t,
					fmt.Sprintf("fixture visibility %s", cleanPath),
					liveProviderRecurrenceDecision{
						Reason: liveProviderRecurrencePostMutationDestinationPathLag,
						Retry:  false,
					},
					quirkOutcomeSoftened,
					statStderr,
				)
				return true
			}

			return false
		},
		"stat", cleanPath,
	)
}

func deleteDisappearanceReady(stdout string, stderr string, err error, unexpected string) bool {
	switch {
	case err == nil:
		return !strings.Contains(stdout, unexpected)
	case isRemoteNotFoundCleanup(stderr):
		return true
	default:
		return false
	}
}

func waitForRemoteDeleteDisappearance(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	unexpected string,
	args ...string,
) (string, string) {
	t.Helper()

	return pollRemoteEventually(
		t,
		cfgPath,
		env,
		driveID,
		remoteDeletePropagationTimeout,
		timingKindRemoteDeleteDisappearance,
		fmt.Sprintf("remote delete disappearance for %q", unexpected),
		func(stdout, stderr string, err error) bool {
			return deleteDisappearanceReady(stdout, stderr, err, unexpected)
		},
		args...,
	)
}

// waitForRemoteFileContent waits up to daemonPollTimeout until a download of
// remotePath, on the drive that env selects or else on the suite's primary
// drive, holds exactly want. A visible name proves nothing about the content:
// a new file is created empty and filled by a second request, so `stat` and
// `ls` answer while the file still has 0 bytes. A test that stops the daemon
// on the name alone can stop it between the two requests and then read an
// empty file (LI-20261110-01).
//
// Every attempt downloads to the same local path, which the wait owns. `get`
// publishes a download by renaming it over its destination and the content is
// read only after a `get` that succeeded, so an attempt never reads an earlier
// attempt's bytes. The wait compares only bytes it downloaded itself, never
// `get`'s stdout. On timeout it prints what the last successful download held
// (summarizeContent) and `get`'s last stderr, which says why the last
// download failed, with every configured drive in it named by role.
func waitForRemoteFileContent(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	remotePath string,
	want string,
) {
	t.Helper()

	localPath := filepath.Join(t.TempDir(), "downloaded")
	lastDownload := "no get succeeded"
	_, stderr, isReady := pollRemoteUntil(
		t,
		cfgPath,
		env,
		"",
		daemonPollTimeout,
		timingKindRemoteWriteVisibility,
		fmt.Sprintf("remote content of %q to equal %s", remotePath, summarizeContent([]byte(want))),
		func(_ string, _ string, err error) bool {
			if err != nil {
				return false
			}

			data, readErr := os.ReadFile(localPath) //nolint:gosec // the file the CLI just downloaded into the test's own temp dir
			if readErr != nil {
				lastDownload = fmt.Sprintf("reading the download failed: %v", readErr)
				return false
			}

			lastDownload = summarizeContent(data)
			return string(data) == want
		},
		"get",
		remotePath,
		localPath,
	)
	if !isReady {
		require.Failf(t, "waitForRemoteFileContent: timed out",
			"after %v waiting for %q to hold %s\nlast download: %s\nlast get stderr: %s",
			daemonPollTimeout, remotePath, summarizeContent([]byte(want)), lastDownload, suiteDriveRoles.Redact(stderr))
	}
}

// readRemoteFileOnce downloads remotePath once, on the drive that env selects
// or else on the suite's primary drive, into localPath and returns its
// content. It asserts nothing, so a poll predicate can call it: a failed get
// or read is logged and reported as false.
func readRemoteFileOnce(t *testing.T, cfgPath string, env map[string]string, remotePath, localPath string) (string, bool) {
	t.Helper()

	if _, _, err := runCLICore(t, cfgPath, env, resolveDriveSelection(env, ""), "get", remotePath, localPath); err != nil {
		t.Logf("waiting for %s: get failed: %v (its output is in the test's debug log)", remotePath, err)
		return "", false
	}

	data, err := os.ReadFile(localPath) //nolint:gosec // the file the CLI just downloaded into the test's own temp dir
	if err != nil {
		t.Logf("waiting for %s: reading the download failed: %v", remotePath, err)
		return "", false
	}

	return string(data), true
}

// getRemoteFile downloads a remote file and returns its content as a string.
// cfgPath must point to a valid config with the drive section; env overrides
// (if non-nil) are forwarded to the CLI child process.
func getRemoteFile(t *testing.T, cfgPath string, env map[string]string, remotePath string) string {
	t.Helper()

	return getRemoteFileForDrive(t, cfgPath, env, resolveDriveSelection(env, ""), remotePath)
}

func getRemoteFileForDrive(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	remotePath string,
) string {
	t.Helper()

	resolvedDriveID := resolveDriveSelection(env, driveID)
	waitForRemoteFixtureSeedVisible(t, cfgPath, env, resolvedDriveID, remotePath)

	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "downloaded")
	pollRemoteEventually(
		t,
		cfgPath,
		env,
		resolvedDriveID,
		remoteWritePropagationTimeout,
		timingKindRemoteWriteVisibility,
		fmt.Sprintf("remote get readability for %q", remotePath),
		func(_ string, _ string, err error) bool {
			return err == nil
		},
		"get",
		remotePath,
		localPath,
	)

	data, readErr := os.ReadFile(localPath) //nolint:gosec // the file the CLI just downloaded into the test's own temp dir
	require.NoError(t, readErr)
	return string(data)
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

func TestDeleteDisappearanceReady(t *testing.T) {
	t.Parallel()

	assert.True(t, deleteDisappearanceReady("other.txt\n", "", nil, "target.txt"))
	assert.False(t, deleteDisappearanceReady("target.txt\n", "", nil, "target.txt"))
	assert.True(t, deleteDisappearanceReady("", "The resource could not be found.", assert.AnError, "target.txt"))
	assert.False(t, deleteDisappearanceReady("", "transport timeout", assert.AnError, "target.txt"))
}
