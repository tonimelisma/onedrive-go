//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

type remoteReadRecoveryAction uint8

const (
	remoteReadNoRecovery remoteReadRecoveryAction = iota
	remoteReadRefreshSharedRootListing
)

func waitForSharedRootListingVisible(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
) {
	t.Helper()

	waitForSharedRootListingVisibleWithin(
		t,
		cfgPath,
		env,
		driveID,
		remoteWritePropagationTimeout,
	)
}

func waitForSharedRootListingVisibleWithin(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
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
		fmt.Sprintf("shared-root listing visibility for %q", driveID),
		func(_ string, _ string, err error) bool {
			return err == nil
		},
		"ls",
		"/",
	)
}

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
	if err == nil || isSharedRootListingProbe(args) {
		return stdout, stderr, err
	}

	if classifyRemoteReadRecoveryAction(driveID, stderr) != remoteReadRefreshSharedRootListing {
		return stdout, stderr, err
	}

	remaining := time.Until(deadline)
	if remaining <= 0 {
		return stdout, stderr, err
	}

	waitForSharedRootListingVisibleWithin(t, cfgPath, env, driveID, remaining)

	return runCLICore(t, cfgPath, env, driveID, args...)
}

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
		if ready(stdout, stderr, err) {
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
			return stdout, stderr
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
			require.Failf(t, "pollRemoteEventually: timed out",
				"after %v waiting for %s via %v\nlast stdout: %s\nlast stderr: %s",
				timeout, description, args, stdout, stderr)
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

func waitForRemoteScopeTransition(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	expected string,
	args ...string,
) (string, string) {
	t.Helper()

	return pollRemoteEventually(
		t,
		cfgPath,
		env,
		driveID,
		remoteScopeTransitionTimeout,
		timingKindRemoteScopeTransition,
		fmt.Sprintf("remote scope transition for %q", expected),
		func(stdout, _ string, err error) bool {
			return err == nil && strings.Contains(stdout, expected)
		},
		args...,
	)
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

	data, readErr := os.ReadFile(localPath)
	require.NoError(t, readErr)
	return string(data)
}

func TestClassifyRemoteReadRecoveryAction(t *testing.T) {
	t.Parallel()

	sharedCID, err := driveid.ConstructShared("user@example.com", "drive-id", "item-id")
	require.NoError(t, err)

	personalCID, err := driveid.NewCanonicalID("personal:user@example.com")
	require.NoError(t, err)

	const sharedRootRoute404 = `Error: resolving "/folder/file.txt": resolve item path "folder/file.txt": list children for segment "folder": graph: HTTP 404 (request-id: req-123): The resource could not be found.`

	require.Equal(t, remoteReadRefreshSharedRootListing, classifyRemoteReadRecoveryAction(sharedCID.String(), sharedRootRoute404))
	require.Equal(t, remoteReadNoRecovery, classifyRemoteReadRecoveryAction(personalCID.String(), sharedRootRoute404))
	require.Equal(t, remoteReadNoRecovery, classifyRemoteReadRecoveryAction(sharedCID.String(), `Error: creating folder "data": graph: HTTP 403: Access denied`))
	require.Equal(t, remoteReadNoRecovery, classifyRemoteReadRecoveryAction("not-a-canonical-id", sharedRootRoute404))
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

func TestDeleteDisappearanceReady(t *testing.T) {
	t.Parallel()

	assert.True(t, deleteDisappearanceReady("other.txt\n", "", nil, "target.txt"))
	assert.False(t, deleteDisappearanceReady("target.txt\n", "", nil, "target.txt"))
	assert.True(t, deleteDisappearanceReady("", "The resource could not be found.", assert.AnError, "target.txt"))
	assert.False(t, deleteDisappearanceReady("", "transport timeout", assert.AnError, "target.txt"))
}
