package devtool

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingRaceStdout and failingRaceStderr are what the fake go test of the
// race step prints when it fails: a test's failure on stdout and a line of
// its own on stderr, both of which the step's log must keep.
const (
	failingRaceStdout = "--- FAIL: TestPlannerExhaustive (0.01s)\n" +
		"    planner_test.go:12: pair 401 diverged\n" +
		"FAIL\n" +
		"FAIL\tgithub.com/tonimelisma/onedrive-go/internal/sync\t0.02s\n"
	failingRaceStderr = "WARNING: DATA RACE\n"
)

func runFailingRaceProfile(t *testing.T, repoRoot string, logDir string) (string, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()

	runner := &fakeRunner{
		runStdoutFunc: func([]string) string { return failingRaceStdout },
		runStderrFunc: func([]string) string { return failingRaceStderr },
		runErr:        assert.AnError,
	}
	summaryPath := filepath.Join(t.TempDir(), "summary.json")
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:        repoRoot,
		Profile:         VerifyRace,
		SummaryJSONPath: summaryPath,
		VerifyLogDir:    logDir,
		Stdout:          stdout,
		Stderr:          stderr,
	})
	require.Error(t, err)

	return summaryPath, stdout, stderr
}

// Validates: R-6.10.36
//
// A failed go test step keeps everything it printed, stdout and stderr, in
// <repo>/.verify-logs/<profile>-<step>.log, and names the file on the line
// after its marker, in the text summary and in the summary JSON.
func TestRunVerifyKeepsTheFullOutputOfAFailedGoTestStep(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	summaryPath, stdout, stderr := runFailingRaceProfile(t, repoRoot, "")

	logPath := filepath.Join(repoRoot, defaultVerifyLogDirName, string(VerifyRace)+"-"+string(raceStepName)+".log")
	data, err := readFile(logPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), failingRaceStdout)
	assert.Contains(t, string(data), failingRaceStderr)

	assert.Contains(t, stderr.String(),
		failedStepMarker(string(VerifyRace), raceStepName)+verifyStepLogPrefix+logPath+"\n",
		"the log's line follows the marker line, which stays as it was")
	assert.Contains(t, stdout.String(), "  log: "+logPath+"\n")

	var summary VerifySummary
	readVerifySummaryFile(t, summaryPath, &summary)
	step := requireVerifySummaryStep(t, summary, raceStepName)
	assert.Equal(t, verifySummaryStatusFail, step.Status)
	assert.Equal(t, logPath, step.LogPath)
}

// Validates: R-6.10.36
//
// A go test step that passes removes its log, so a passing run leaves no
// file behind, and the summary names none.
func TestRunVerifyRemovesTheLogOfAPassingGoTestStep(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	summaryPath := filepath.Join(t.TempDir(), "summary.json")
	stdout := &bytes.Buffer{}
	err := RunVerify(context.Background(), &fakeRunner{}, &VerifyOptions{
		RepoRoot:        repoRoot,
		Profile:         VerifyRace,
		SummaryJSONPath: summaryPath,
		Stdout:          stdout,
		Stderr:          &bytes.Buffer{},
	})
	require.NoError(t, err)

	entries, err := os.ReadDir(filepath.Join(repoRoot, defaultVerifyLogDirName))
	require.NoError(t, err)
	assert.Empty(t, entries)
	assert.NotContains(t, stdout.String(), "  log: ")

	var summary VerifySummary
	readVerifySummaryFile(t, summaryPath, &summary)
	assert.Empty(t, requireVerifySummaryStep(t, summary, raceStepName).LogPath)
}

// Validates: R-6.10.36
//
// --verify-log-dir moves the kept logs: the failed step's file is written
// there and nothing is written under the repository.
func TestRunVerifyKeepsStepLogsInTheNamedDirectory(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	logDir := filepath.Join(t.TempDir(), "logs")
	summaryPath, _, stderr := runFailingRaceProfile(t, repoRoot, logDir)

	logPath := filepath.Join(logDir, string(VerifyRace)+"-"+string(raceStepName)+".log")
	data, err := readFile(logPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), failingRaceStdout)
	assert.Contains(t, stderr.String(), verifyStepLogPrefix+logPath+"\n")

	_, err = os.Stat(filepath.Join(repoRoot, defaultVerifyLogDirName))
	require.ErrorIs(t, err, os.ErrNotExist)

	var summary VerifySummary
	readVerifySummaryFile(t, summaryPath, &summary)
	assert.Equal(t, logPath, requireVerifySummaryStep(t, summary, raceStepName).LogPath)
}

// Validates: R-6.10.36
//
// A step's log is named by the profile and the step, and by the shard of a
// sharded run, so the shards of one job never write one file.
func TestStepLogPathNamesTheProfileTheStepAndTheShard(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	assert.Equal(t, filepath.Join(dir, "race-sync-race.log"),
		newStepLogs(dir, VerifyRace, verifyShard{}).path(raceStepName))
	assert.Equal(t, filepath.Join(dir, "quick-short-unit-tests-shard-2-of-3.log"),
		newStepLogs(dir, VerifyQuick, verifyShard{index: 2, count: 3}).path(shortUnitTestsStepName))
}

// Validates: R-6.10.36
//
// A step that runs no go test keeps no log, even when it fails.
func TestRunVerifyKeepsNoLogForAStepThatRunsNoGoTest(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	stderr := &bytes.Buffer{}
	err := RunVerify(context.Background(), &fakeRunner{runErr: assert.AnError}, &VerifyOptions{
		RepoRoot: repoRoot,
		Profile:  VerifyLint,
		Stdout:   &bytes.Buffer{},
		Stderr:   stderr,
	})
	require.Error(t, err)
	assert.NotContains(t, stderr.String(), verifyStepLogPrefix)

	_, err = os.Stat(filepath.Join(repoRoot, defaultVerifyLogDirName))
	require.ErrorIs(t, err, os.ErrNotExist)
}
