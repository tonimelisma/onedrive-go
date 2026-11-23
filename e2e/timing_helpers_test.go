//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const e2eSkipSuiteScrubEnvVar = "ONEDRIVE_E2E_SKIP_SUITE_SCRUB"

func recordTimingEvent(
	t *testing.T,
	kind string,
	description string,
	driveID string,
	args []string,
	timeout time.Duration,
	duration time.Duration,
	attempts int,
	outcome string,
) {
	t.Helper()

	if suiteTimingRecorder == nil {
		return
	}

	if err := suiteTimingRecorder.record(e2eTimingEvent{
		TestName:    t.Name(),
		Kind:        kind,
		Description: description,
		DriveID:     driveID,
		Command:     strings.Join(args, " "),
		TimeoutMS:   timeout.Milliseconds(),
		DurationMS:  duration.Milliseconds(),
		Attempts:    attempts,
		Outcome:     outcome,
	}); err != nil {
		t.Logf("warning: cannot record timing event: %v", err)
	}
}

// Validates: R-6.10.27
//
// The timing and quirk summaries and the debug logs are CI artifacts of a
// public repository, file names included. A recorded drive, command line or
// test name naming the configured drive is written with the drive's role
// instead, and a debug log of a test named after the drive is named by the
// role. The messages name files by their redacted names and never print what
// a file holds.
func TestE2ERecordersRecordRolesNotDrives(t *testing.T) {
	dir := t.TempDir()
	timing, err := newE2ETimingRecorder(dir, suiteDriveRoles)
	require.NoError(t, err)
	quirks, err := newE2EQuirkRecorder(dir, suiteDriveRoles)
	require.NoError(t, err)

	namedAfterDrive := "TestNamedAfterADrive/" + drive
	require.NoError(t, timing.record(e2eTimingEvent{
		TestName:    namedAfterDrive,
		Kind:        timingKindRemoteWriteVisibility,
		Description: "write on " + drive,
		DriveID:     drive,
		Command:     "stat --drive " + drive + " /roles.txt",
		TimeoutMS:   1000,
		DurationMS:  10,
		Attempts:    1,
		Outcome:     timingOutcomeSuccess,
	}))
	require.NoError(t, quirks.record(&e2eQuirkEvent{
		Phase:      quirkPhaseAuthPreflight,
		TestName:   "TestE2E_AuthPreflight_Fast/" + sanitizeTestName(drive),
		Operation:  "GET /me/drive drive=" + drive,
		Source:     quirkSourceAuthPreflight,
		Outcome:    quirkOutcomeFailed,
		FinalError: "auth preflight failed for " + drive,
	}))
	require.NoError(t, appendDebugLogChunk(filepath.Join(dir, debugLogFileName(namedAfterDrive)), "CMD: ls --drive "+drive+" /\n"))

	_, account, ok := strings.Cut(drive, ":")
	require.True(t, ok)
	account = strings.ToLower(account)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := suiteDriveRoles.Redact(entry.Name())
		names = append(names, name)
		// Not NotContains or Contains: they print both strings on failure,
		// and the account must not reach the public log.
		//nolint:testifylint // NotContainsf would print the account
		assert.Falsef(t, strings.Contains(strings.ToLower(entry.Name()), account), "the file name %s names the account", name)
		data := strings.ToLower(string(mustReadFile(t, filepath.Join(dir, entry.Name()))))
		//nolint:testifylint // NotContainsf would print the account
		assert.Falsef(t, strings.Contains(data, account), "%s names the account", name)
		//nolint:testifylint // Containsf would print the file, which may hold the account
		assert.Truef(t, strings.Contains(data, "drive-1"), "%s names the drive by role", name)
	}
	assert.Contains(t, names, "TestNamedAfterADrive_drive-1.log", "the debug log of a test named after the drive is named by role")
}
