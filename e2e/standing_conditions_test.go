//go:build e2e

package e2e

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tonimelisma/onedrive-go/testutil"
)

// standingBaselineLogName names the suite debug log that holds the output of
// the baseline passes.
const standingBaselineLogName = "standing-baseline"

// standingConditionSettledReason is what the debug log and the quirk record
// say about a pass settleStandingSyncConditions read as clean.
const standingConditionSettledReason = "the pass left only conditions the account held before the suite started"

// recordStandingSyncConditions runs, before any test, one download-only sync
// of each suite drive into a throwaway folder and records what the account
// itself leaves unsynced (testutil.StandingSyncConditions). A shared folder
// whose owner took it away is still not synced on every pass, and the sync
// command rightly exits 1 on each (R-6.6.28); no test made that condition and
// no test can clear it. settleStandingSyncConditions then gives a test the
// pass it would have seen without it.
//
// Its output names a drive by role and gives counts only: it runs from
// TestMain, whose output reaches the public CI log. What the passes printed
// goes to the suite's debug log.
//
// A drive whose baseline pass cannot be read records nothing and the suite
// goes on: its tests then see the command's own exit code. One finding stops
// the suite, items left unsynced on a line that names no mount, because no
// later pass could tell them from a test's own left work.
func recordStandingSyncConditions(cleanupIsolation func(), tmpDir string, driveIDs ...string) {
	for _, driveID := range driveIDs {
		if driveID == "" {
			continue
		}

		role := suiteDriveRoles.Role(driveID)
		stdout, err := runStandingBaselinePass(tmpDir, driveID)
		if err == nil {
			continue
		}

		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != syncExitLeftWork {
			fmt.Fprintf(os.Stderr, "E2E standing conditions: drive=%s nothing recorded, the baseline pass did not finish: %s\n",
				role, suiteDriveRoles.Redact(err.Error()))
			continue
		}

		var pass testutil.StandingSyncConditions
		pass.RecordBaseline(stdout)
		switch {
		case pass.Unattributed() > 0:
			abortSuitef(cleanupIsolation, tmpDir,
				"E2E standing conditions: drive=%s leaves %d items unsynced on a first download-only pass that names no mount; "+
					"no test's own left work could be told from them. Clear them in the test account (%s has the pass).",
				role, pass.Unattributed(), debugLogPath(standingBaselineLogName))
		case pass.Empty():
			fmt.Fprintf(os.Stderr, "E2E standing conditions: drive=%s nothing recorded, the baseline pass left work that is no standing condition\n", role)
		default:
			suiteStandingConditions.RecordBaseline(stdout)
			fmt.Fprintf(os.Stderr,
				"E2E standing conditions: drive=%s %d mounts leave %d items unsynced before any test; a pass that leaves only these reads as clean\n",
				role, pass.Mounts(), pass.Items())
		}
	}
}

// runStandingBaselinePass syncs one drive download-only into a new folder
// under tmpDir, with its own data dir and home like a test's own sync
// (writeSyncConfig), and removes the folder again. It returns the pass's
// stdout and the command's error, and appends what the pass printed to the
// suite's debug log.
func runStandingBaselinePass(tmpDir, driveID string) (string, error) {
	root, err := os.MkdirTemp(tmpDir, standingBaselineLogName+"-*")
	if err != nil {
		return "", fmt.Errorf("creating the baseline folder: %w", err)
	}
	defer func() {
		if removeErr := os.RemoveAll(root); removeErr != nil {
			fmt.Fprintf(os.Stderr, "WARNING: removing the E2E standing-conditions folder %s: %v\n", root, removeErr)
		}
	}()

	dataHome := filepath.Join(root, "data")
	dataDir := filepath.Join(dataHome, "onedrive-go")
	home := filepath.Join(root, "home")
	syncDir := filepath.Join(root, "sync")
	for _, dir := range [...]string{dataDir, home, syncDir} {
		if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
			return "", fmt.Errorf("creating the baseline folder: %w", mkErr)
		}
	}

	tokenName := testutil.TokenFileName(driveID)
	testutil.CopyFile(filepath.Join(testDataDir, tokenName), filepath.Join(dataDir, tokenName), 0o600)
	testutil.CopyCatalogFile(testDataDir, dataDir)

	cfgPath := filepath.Join(root, "config.toml")
	config := fmt.Sprintf("[%q]\nsync_dir = %q\n", driveID, syncDir)
	if writeErr := os.WriteFile(cfgPath, []byte(config), 0o600); writeErr != nil {
		return "", fmt.Errorf("writing the baseline config: %w", writeErr)
	}

	env := map[string]string{
		"XDG_DATA_HOME": dataHome,
		"HOME":          home,
	}
	fullArgs, stdout, stderr, runErr := execCLI(cfgPath, env, driveID, "sync", "--download-only")
	logPath := debugLogPath(standingBaselineLogName)
	if logErr := appendDebugLogChunk(logPath, cliExecutionLogEntry(fullArgs, stdout, stderr)); logErr != nil {
		fmt.Fprintf(os.Stderr, "WARNING: cannot write the E2E standing-conditions log: %v\n", logErr)
	}

	return stdout, runErr
}

// settleStandingSyncConditions gives a test the one-shot sync pass it would
// have seen in an account without the suite's standing conditions. When a
// pass exited 1 and left nothing but those, it returns the output the command
// prints without them and no error; every other result is returned as it is.
// The pass as the command printed it is already in the test's debug log; the
// note there and the quirk record say that the test read it as clean.
func settleStandingSyncConditions(
	t *testing.T,
	fullArgs []string,
	stdout, stderr string,
	err error,
) (string, string, error) {
	t.Helper()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != syncExitLeftWork {
		return stdout, stderr, err
	}

	settledStdout, settledStderr, settled := suiteStandingConditions.Settle(stdout, stderr)
	if !settled {
		return stdout, stderr, err
	}

	note := "--- STANDING CONDITIONS ---\n" + standingConditionSettledReason + "; the test reads it as a pass that left nothing\n\n"
	if logErr := appendDebugLogChunk(debugLogPath(t.Name()), note); logErr != nil {
		t.Logf("warning: cannot write debug log: %v", logErr)
	}
	recordQuirkEvent(t, &e2eQuirkEvent{
		Phase:         quirkPhaseCLICommand,
		TestName:      t.Name(),
		Operation:     strings.Join(fullArgs, " "),
		Source:        quirkSourceStandingCondition,
		QuirkOrReason: standingConditionSettledReason,
		Outcome:       quirkOutcomeSoftened,
	})

	return settledStdout, settledStderr, nil
}
