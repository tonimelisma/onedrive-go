package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	"github.com/tonimelisma/onedrive-go/testutil"
)

// This file holds what the live suite prints of a command, a debug event or
// an account, as deterministic functions the untagged quick lane tests: CI
// logs are public, so a failure message or a test log names every account,
// drive, item and fixture of the test accounts by role, and prints a
// command's output masked and bounded. The suite's own roles
// (newSuiteDriveRoles) come from what it already holds: the configured
// drives, the fixtures of its environment and the catalog the test
// credentials bring.

// printedOutputLimit bounds how much of a command's output a failure message
// or a test log prints: its last 4 KiB, where the command's error and
// summary stand. The test's debug log holds the whole output.
const printedOutputLimit = 4 << 10

// printableWith is text as a failure message or a test log prints it: masked
// by roles, then cut to its last printedOutputLimit bytes on a character
// boundary, the cut marked. An empty text stays empty.
func printableWith(roles testutil.DriveRoles, text string) string {
	return lastBytesOf(roles.Redact(text), printedOutputLimit, "the test's debug log")
}

// debugLogDumpLimit bounds how much of a failed test's debug log the job log
// shows: its last 64 KiB, where the failing command stands. The log file, a
// CI artifact, holds the whole of it.
const debugLogDumpLimit = 64 << 10

// debugLogDump is what a failed test prints of its debug log: the file that
// holds the whole log, then the log masked by roles (as it was when it was
// written) and cut to its last debugLogDumpLimit bytes.
func debugLogDump(roles testutil.DriveRoles, logPath, log string) string {
	return fmt.Sprintf("=== DEBUG LOG DUMP (%s) ===\n%s", logPath, lastBytesOf(roles.Redact(log), debugLogDumpLimit, logPath))
}

// lastBytesOf is text cut to its last limit bytes on a character boundary,
// the cut marked with where the whole text is.
func lastBytesOf(text string, limit int, wholeTextAt string) string {
	if len(text) <= limit {
		return text
	}

	start := len(text) - limit
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}

	return fmt.Sprintf("[first %d bytes cut; %s holds them]\n%s", start, wholeTextAt, text[start:])
}

// outputAssertionFailure is the message of an output assertion that failed:
// what it looked for and the output, both masked, the output bounded.
func outputAssertionFailure(roles testutil.DriveRoles, output, want string, wantContained bool) string {
	verb := "does not contain"
	if !wantContained {
		verb = "contains"
	}

	return fmt.Sprintf("the output %s %q\noutput: %s", verb, roles.Redact(want), printableWith(roles, output))
}

// outputMismatch is the message of an equality on a value a command's
// output gave that failed: the value and the one expected, both masked and
// bounded.
func outputMismatch(roles testutil.DriveRoles, want, got string) string {
	return fmt.Sprintf("the output gives %q, want %q", printableWith(roles, got), printableWith(roles, want))
}

// debugEventsSummaryLimit is how many of a stream's last events a failure
// prints.
const debugEventsSummaryLimit = 10

// debugEventsSummary is what a failure prints of a debug-event stream: how
// many events it holds, then one line for each of the last
// debugEventsSummaryLimit (debugEventSummary).
func debugEventsSummary(roles testutil.DriveRoles, events []syncengine.DebugEvent) string {
	if len(events) == 0 {
		return "no events"
	}

	shown := events[max(0, len(events)-debugEventsSummaryLimit):]
	var summary strings.Builder
	fmt.Fprintf(&summary, "%d events, the last %d:", len(events), len(shown))
	for i := range shown {
		summary.WriteString("\n  " + debugEventSummary(roles, &shown[i]))
	}

	return summary.String()
}

// debugEventSummary is one event's line: its type, and its observer, note and
// path masked by roles, where a throttle target's drive in the note is named
// by role or masked (drive:<id>), then its error masked and bounded. The
// event's DriveID and ScopeKey fields are not printed: they name the drive.
func debugEventSummary(roles testutil.DriveRoles, event *syncengine.DebugEvent) string {
	line := string(event.Type)
	for _, field := range []struct{ name, value string }{
		{name: "observer", value: event.Observer},
		{name: "note", value: event.Note},
		{name: "path", value: event.Path},
	} {
		if field.value != "" {
			line += " " + field.name + "=" + field.value
		}
	}
	line = roles.Redact(line)
	if event.Error != "" {
		line += " error=" + printableWith(roles, event.Error)
	}

	return line
}

// catalogIdentities lists what the suite's catalog holds about each
// configured drive and its account, by the drive's role: the drive's Graph
// ID as drive-N-id (the drive's record and the account's primary drive), and
// the account's user ID, display name and organization as drive-N-account-id,
// -name and -org. The catalog comes with the test credentials, so learning
// them takes no request to OneDrive. It also lists the roles of the drives
// the catalog holds no record of, neither of the drive nor of its account.
func catalogIdentities(
	catalog *config.Catalog, roles testutil.DriveRoles, driveIDs ...string,
) ([]testutil.IdentityRole, []string) {
	var identities []testutil.IdentityRole
	var undescribed []string
	for _, driveID := range driveIDs {
		cid, err := driveid.NewCanonicalID(driveID)
		if err != nil {
			continue
		}
		role := roles.Role(driveID)

		accountCID := cid
		drive, driveFound := catalog.DriveByCanonicalID(cid)
		if driveFound {
			identities = append(identities, testutil.IdentityRole{Value: drive.RemoteDriveID, Role: role + "-id"})
			if owner, ownerErr := driveid.NewCanonicalID(drive.OwnerAccountCanonical); ownerErr == nil {
				accountCID = owner
			}
		}
		account, accountFound := catalog.AccountByCanonicalID(accountCID)
		if !driveFound && !accountFound {
			undescribed = append(undescribed, role)
		}
		if accountFound {
			identities = append(identities,
				testutil.IdentityRole{Value: account.PrimaryDriveID, Role: role + "-id"},
				testutil.IdentityRole{Value: account.UserID, Role: role + "-account-id"},
				testutil.IdentityRole{Value: account.DisplayName, Role: role + "-account-name", IsName: true},
				testutil.IdentityRole{Value: account.OrgName, Role: role + "-account-org", IsName: true},
			)
		}
	}

	return identities, undescribed
}

// newSuiteDriveRoles names the suite's drives, what the catalog holds about
// them and the fixtures of cfg by role. The catalog's come first, so a
// fixture on a configured drive's Graph drive is named after the drive. It
// also returns the lines the suite writes to stderr about what it did not
// learn, which show in a CI log what the run's environment provides: each
// drive role the catalog does not describe, each name too short to learn,
// and each fixture variable left unset so that its default applied. They
// name roles and variables, never a value.
func newSuiteDriveRoles(cfg *testutil.LiveTestConfig, catalog *config.Catalog) (testutil.DriveRoles, []string) {
	drives := []string{cfg.PrimaryDrive, cfg.SecondaryDrive}
	identities, undescribed := catalogIdentities(catalog, testutil.NewDriveRoles(drives...), drives...)
	roles := testutil.NewSuiteRoles(drives, append(identities, testutil.FixtureIdentities(&cfg.Fixtures)...))

	notes := make([]string, 0, len(undescribed)+len(roles.UnlearnedNameRoles())+len(cfg.DefaultedFixtureVariables))
	for _, role := range undescribed {
		notes = append(notes, "E2E roles: the catalog does not describe "+role+
			"; its IDs are masked only where the product's output puts an identifier")
	}
	for _, role := range roles.UnlearnedNameRoles() {
		notes = append(notes, "E2E roles: the "+role+" name is too short to learn and stays readable")
	}
	for _, variable := range cfg.DefaultedFixtureVariables {
		notes = append(notes, "E2E fixtures: "+variable+" is unset; its default applies")
	}

	return roles, notes
}

// Invented values in the shapes the suite meets.
const (
	printedFirstDrive      = "personal:printed.one@example.test"
	printedSecondDrive     = "business:printed_two@example.test"
	printedFirstGraphDrive = "00c1d2e3f4a5b607"
	printedUserID          = "printed-user-0001"
	printedDisplayName     = "Printed Person"
	printedFolderSelector  = "shared:printed.one@example.test:0e1f2a3b4c5d6978:0E1F2A3B4C5D6978!321"
)

func printedCatalog(t *testing.T) *config.Catalog {
	t.Helper()

	catalog := &config.Catalog{}
	catalog.UpsertDrive(&config.CatalogDrive{CanonicalID: printedFirstDrive, RemoteDriveID: printedFirstGraphDrive})
	catalog.UpsertAccount(&config.CatalogAccount{
		CanonicalID: printedFirstDrive, UserID: printedUserID, DisplayName: printedDisplayName,
		OrgName: "Printed Org", PrimaryDriveID: printedFirstGraphDrive,
	})
	cid, err := driveid.NewCanonicalID(printedFirstDrive)
	require.NoError(t, err)
	_, found := catalog.AccountByCanonicalID(cid)
	require.True(t, found, "the planted catalog holds the first drive's account")

	return catalog
}

// Validates: R-6.10.27
//
// The suite's roles come from what it holds: its drives, the fixtures of its
// environment and the catalog of its credentials. A drive's Graph ID, its
// account's user ID, display name and organization, and a fixture's selector
// are printed by role.
func TestNewSuiteDriveRolesNamesCatalogAndFixtureValuesByRole(t *testing.T) {
	t.Parallel()

	cfg := testutil.LiveTestConfig{
		PrimaryDrive:   printedFirstDrive,
		SecondaryDrive: printedSecondDrive,
		Fixtures: testutil.LiveFixtures{
			WritableSharedFolderSelector: printedFolderSelector, StandaloneSharedFolderName: "Pf",
		},
		DefaultedFixtureVariables: []string{"ONEDRIVE_TEST_SHORTCUT_WRITABLE_NAME"},
	}
	roles, notes := newSuiteDriveRoles(&cfg, printedCatalog(t))

	for value, role := range map[string]string{
		printedFirstGraphDrive:     "drive-1-id",
		"C1D2E3F4A5B607":           "drive-1-id",
		printedUserID:              "drive-1-account-id",
		printedDisplayName:         "drive-1-account-name",
		"Printed Org":              "drive-1-account-org",
		printedFolderSelector:      "writable-shared-folder",
		"0E1F2A3B4C5D6978!321":     "writable-shared-folder-item",
		"printed_two@example.test": "drive-2-account",
	} {
		assert.Equal(t, role, roles.Redact(value), "the role of a planted %s", role)
	}

	identities, undescribed := catalogIdentities(&config.Catalog{}, roles, printedFirstDrive)
	assert.Empty(t, identities, "a catalog without a record of the drive teaches nothing")
	assert.Equal(t, []string{"drive-1"}, undescribed)

	assert.Equal(t, []string{
		"E2E roles: the catalog does not describe drive-2; its IDs are masked only where the product's output puts an identifier",
		"E2E roles: the standalone-shared-folder name is too short to learn and stays readable",
		"E2E fixtures: ONEDRIVE_TEST_SHORTCUT_WRITABLE_NAME is unset; its default applies",
	}, notes, "the suite says what it did not learn by role and variable, never by value")
}

// Validates: R-6.10.27
//
// A command's output in a failure message is masked and bounded: its last
// printedOutputLimit bytes, cut on a character boundary and marked, so a
// --debug log of megabytes cannot flood the public CI log.
func TestPrintableWithMasksThenBoundsOutput(t *testing.T) {
	t.Parallel()

	roles := testutil.NewDriveRoles(printedFirstDrive)

	assert.Equal(t, "listing of drive-1 failed", printableWith(roles, "listing of "+printedFirstDrive+" failed"))
	assert.Empty(t, printableWith(roles, ""), "an empty output stays empty, so Empty and NotEmpty still hold")

	long := strings.Repeat("ä", printedOutputLimit) + "\nError: printed.one@example.test was not found on OneDrive."
	printed := printableWith(roles, long)
	assert.True(t, strings.HasPrefix(printed, "[first "), "the cut is marked: %.60s", printed)
	assert.Contains(t, printed, "the test's debug log holds them]\n")
	assert.True(t, strings.HasSuffix(printed, "Error: drive-1-account was not found on OneDrive."))
	assert.LessOrEqual(t, len(printed), printedOutputLimit+len("[first 999999 bytes cut; the test's debug log holds them]\n"))
	assert.True(t, utf8.ValidString(printed), "the cut keeps whole characters")
}

// Validates: R-6.10.27
//
// A failed test prints its debug log masked and bounded: the last
// debugLogDumpLimit bytes, after the name of the file that holds the whole.
func TestDebugLogDumpNamesTheFileAndBoundsTheLog(t *testing.T) {
	t.Parallel()

	roles := testutil.NewDriveRoles(printedFirstDrive)
	logPath := "/logs/TestPlanted.log"

	assert.Equal(t, "=== DEBUG LOG DUMP (/logs/TestPlanted.log) ===\nCMD: ls --drive drive-1\n",
		debugLogDump(roles, logPath, "CMD: ls --drive "+printedFirstDrive+"\n"))

	dump := debugLogDump(roles, logPath, strings.Repeat("x", debugLogDumpLimit)+"\nError: "+printedFirstDrive)
	assert.True(t, strings.HasPrefix(dump, "=== DEBUG LOG DUMP (/logs/TestPlanted.log) ===\n[first "),
		"the cut is marked: %.80s", dump)
	assert.Contains(t, dump, "bytes cut; /logs/TestPlanted.log holds them]\n")
	assert.True(t, strings.HasSuffix(dump, "\nError: drive-1"), "the end of the log stays, masked")
	assert.Less(t, len(dump), debugLogDumpLimit+len(logPath)*2+len("=== DEBUG LOG DUMP () ===\n[first 99999 bytes cut;  holds them]\n"))
}

// Validates: R-6.10.27
func TestOutputAssertionFailureMasksBoth(t *testing.T) {
	t.Parallel()

	roles := testutil.NewDriveRoles(printedFirstDrive)

	assert.Equal(t, "the output does not contain \"drive-1\"\noutput: Up to date: drive-1-account",
		outputAssertionFailure(roles, "Up to date: printed.one@example.test", printedFirstDrive, true))
	assert.Equal(t, "the output contains \"Error\"\noutput: Error: drive-1",
		outputAssertionFailure(roles, "Error: "+printedFirstDrive, "Error", false))
	assert.Equal(t, `the output gives "drive-1-account", want "missing_login"`,
		outputMismatch(roles, "missing_login", "printed.one@example.test"))
}

// Validates: R-6.10.27
//
// A failure prints a debug-event stream as a summary: the count, then the
// type, observer, note and path of each of the last events, masked, with an
// error masked and bounded. An event's drive ID and scope key fields are not
// printed, and a throttle target's drive in a note is masked.
func TestDebugEventsSummaryNamesTypesNotDrives(t *testing.T) {
	t.Parallel()

	roles := testutil.NewSuiteRoles([]string{printedFirstDrive},
		[]testutil.IdentityRole{{Value: printedFirstGraphDrive, Role: "drive-1-id"}})
	events := make([]syncengine.DebugEvent, 0, debugEventsSummaryLimit+3)
	for range debugEventsSummaryLimit + 1 {
		events = append(events, syncengine.DebugEvent{Type: "observer_started", Observer: "remote", DriveID: printedFirstGraphDrive})
	}
	events = append(events, syncengine.DebugEvent{
		At: time.Unix(0, 0), Type: "websocket_connect_failed", Observer: "remote", DriveID: "0e1f2a3b4c5d6978",
		Note: "mount_root", Path: "e2e-folder", Error: "GET https://graph.microsoft.com/v1.0/drives/0e1f2a3b4c5d6978/items/root: 503",
	}, syncengine.DebugEvent{Type: "cold_start_allowance_raised", DriveID: "0e1f2a3b4c5d6978", Note: "drive:0e1f2a3b4c5d6978"})

	summary := debugEventsSummary(roles, events)

	assert.True(t, strings.HasPrefix(summary, "13 events, the last 10:"), "summary: %s", summary)
	assert.Contains(t, summary, "websocket_connect_failed observer=remote note=mount_root path=e2e-folder "+
		"error=GET https://graph.microsoft.com/v1.0/drives/masked/items/root: 503")
	assert.Contains(t, summary, "cold_start_allowance_raised note=drive:masked", "a throttle target's drive is masked")
	assert.NotContains(t, summary, "0e1f2a3b4c5d6978")
	assert.NotContains(t, summary, printedFirstGraphDrive)
	assert.Equal(t, debugEventsSummaryLimit, strings.Count(summary, "\n  "), "one line for each of the last events")
	assert.Equal(t, "no events", debugEventsSummary(roles, nil))
}
