package devtool

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testEngineReplay  = "ONEDRIVE_GO_MODEL_SEED=7001 go test -run '^TestRunOnce_RandomizedBidirectionalEditsConverge$' ./internal/sync"
	testPlannerReplay = "ONEDRIVE_GO_PLANNER_SEED=12 go test -race -run '^TestPlannerModel_RandomTriplesConverge$' ./internal/sync/internal/syncplan"
)

// seedReplayJSONLine is the -json event go test prints for a replay line a
// subtest of test logged.
func seedReplayJSONLine(t *testing.T, test string, command string) string {
	t.Helper()

	line, err := json.Marshal(map[string]string{
		"Action":  goTestActionOutput,
		"Package": repoModulePath + "/internal/sync",
		"Test":    test + "/seed=1",
		"Output":  "    test_helpers_test.go:480: " + seedReplayPrefix + command + "\n",
	})
	require.NoError(t, err)

	return string(line) + "\n"
}

// Validates: R-6.10.25
//
// The one reader of replay lines finds them in a raw log behind CI's
// timestamps and in -json output, with and without -race, two families in one
// log, each command once; a line that only looks like one is no replay.
func TestSeedReplaysFromLog(t *testing.T) {
	t.Parallel()

	log := "2026-11-14T10:20:00.1Z === RUN   TestRunOnce_RandomizedBidirectionalEditsConverge/seed=7001\n" +
		"2026-11-14T10:21:00.1Z         engine_model_based_test.go:99: seed 7001; operations:\n" +
		"2026-11-14T10:21:00.2Z         test_helpers_test.go:480: " + seedReplayPrefix + testEngineReplay + "\n" +
		seedReplayJSONLine(t, "TestPlannerModel_RandomTriplesConverge", testPlannerReplay) +
		seedReplayJSONLine(t, "TestPlannerModel_RandomTriplesConverge", testPlannerReplay) +
		"    " + seedReplayPrefix + testEngineReplay + " and more\n" +
		"    " + seedReplayPrefix + "ONEDRIVE_GO_MODEL_SEED=$(id) go test -run '^TestRunOnce_RandomizedBidirectionalEditsConverge$' ./internal/sync\n" +
		"    " + seedReplayPrefix + "go test -run '^TestRunOnce_RandomizedBidirectionalEditsConverge$' ./internal/sync\n" +
		"    " + seedReplayPrefix + "ONEDRIVE_GO_MODEL_SEED=1.5 go test -run '^TestRunOnce_RandomizedBidirectionalEditsConverge$' ./internal/sync\n" +
		"    " + seedReplayPrefix + "ONEDRIVE_GO_MODEL_SEED=1 go test -run ^TestRunOnce_RandomizedBidirectionalEditsConverge$ ./internal/sync\n" +
		"    " + seedReplayPrefix + `ONEDRIVE_GO_MODEL_SEED=1 go test -run '^TestRunOnce_RandomizedBidirectionalEditsConverge$' ./internal/sync\n"}` + "\n"

	replays := seedReplaysFromLog(log)
	require.Len(t, replays, 2)
	assert.Equal(t, seedReplay{
		assignments: []seedAssignment{{name: "ONEDRIVE_GO_MODEL_SEED", value: "7001"}},
		test:        "TestRunOnce_RandomizedBidirectionalEditsConverge",
		pkg:         "./internal/sync",
	}, replays[0])
	assert.Equal(t, seedReplay{
		assignments: []seedAssignment{{name: "ONEDRIVE_GO_PLANNER_SEED", value: "12"}},
		race:        true,
		test:        "TestPlannerModel_RandomTriplesConverge",
		pkg:         "./internal/sync/internal/syncplan",
	}, replays[1])
	assert.Empty(t, seedReplaysFromLog("ok  \t"+repoModulePath+"/internal/sync\t3.1s\n"))
}

// Validates: R-6.10.25
//
// A replay's command is the line's command again, whatever number of
// assignments it holds; its key is the test's package-qualified key, the
// separator, and the family's variable and seed; its environment is every
// assignment.
func TestSeedReplayCommandRoundTrips(t *testing.T) {
	t.Parallel()

	for _, command := range []string{
		testEngineReplay,
		testPlannerReplay,
		"ONEDRIVE_GO_MODEL_SEED=3 ONEDRIVE_GO_DISPATCH_ORDER=reverse go test -race -run '^TestModel_ChaosGraphConverges$' ./internal/sync",
	} {
		replay, ok := parseSeedReplay(command)
		require.True(t, ok, command)
		assert.Equal(t, command, replay.command())
		assert.Equal(t, seedReplayPrefix+command, replay.line())
	}

	replay, ok := parseSeedReplay("ONEDRIVE_GO_MODEL_SEED=3 ONEDRIVE_GO_DISPATCH_ORDER=reverse go test -race -run '^TestModel_ChaosGraphConverges$' ./internal/sync")
	require.True(t, ok)
	assert.Equal(t, "internal/sync.TestModel_ChaosGraphConverges/ONEDRIVE_GO_MODEL_SEED=3", replay.key())
	assert.Equal(t, []string{"ONEDRIVE_GO_MODEL_SEED=3", "ONEDRIVE_GO_DISPATCH_ORDER=reverse"}, replay.env())
	assert.True(t, replay.replays(goTestFailure{pkg: repoModulePath + "/internal/sync", test: "TestModel_ChaosGraphConverges/seed=3"}))
	assert.True(t, replay.replays(goTestFailure{test: "TestModel_ChaosGraphConverges"}), "a log that never named the package")
	assert.False(t, replay.replays(goTestFailure{pkg: "./internal/multisync", test: "TestModel_ChaosGraphConverges"}))
	assert.False(t, replay.replays(goTestFailure{pkg: "./internal/sync", test: "TestPlannerModel_RandomTriplesConverge"}))
}

const (
	// testCrashFamily is the crash family's test, and testTimedOutSeed the
	// seed of it that go test's deadline ends in seedDeadlineLog.
	testCrashFamily  = "TestModel_CrashAtSeededPointsRecovers"
	testTimedOutSeed = "318011"
	// testOtherCrashReplay is the replay line another crash seed printed.
	testOtherCrashReplay = "ONEDRIVE_GO_MODEL_SEED=318012 go test -run '^" + testCrashFamily + "$' ./internal/sync"
)

// seedDeadlineLog is the raw log of a seeds run that go test's deadline ended
// while testTimedOutSeed ran: the alarm panics in a goroutine of its own, no
// cleanup runs, so the seed prints no replay line, and the panic lists its
// subtest as running. With otherReplay, an earlier seed of the same test
// failed and printed that line.
func seedDeadlineLog(otherReplay string) string {
	lines := []string{}
	if otherReplay != "" {
		lines = append(lines,
			"        test_helpers_test.go:480: "+seedReplayPrefix+otherReplay,
			"    --- FAIL: "+testCrashFamily+"/seed=318012 (61.00s)")
	}
	lines = append(lines,
		"panic: test timed out after 30m0s",
		"\trunning tests:",
		"\t\t"+testCrashFamily+" (30m0s)",
		"\t\t"+testCrashFamily+"/seed="+testTimedOutSeed+" (29m10s)",
		"",
		"goroutine 7 [running]:",
		"testing.(*M).startAlarm.func1()",
		"FAIL\t"+repoModulePath+"/internal/sync\t1800.123s",
	)

	return strings.Join(lines, "\n") + "\n"
}

// goTestReportsPass is the -v output of a go test of ./internal/sync in which
// test, a full test name, passed.
func goTestReportsPass(test string) func([]string) string {
	return func([]string) string {
		return "=== RUN   " + test + "\n--- PASS: " + test + " (1.00s)\nPASS\n" +
			"ok  \t" + repoModulePath + "/internal/sync\t1.0s\n"
	}
}

// rerunFailedTest runs `verify failed-test` on log against runner.
func rerunFailedTest(t *testing.T, runner *fakeRunner, log string) error {
	t.Helper()

	repoRoot := t.TempDir()
	logPath := filepath.Join(repoRoot, "ci.log")
	require.NoError(t, os.WriteFile(logPath, []byte(log), 0o600))

	return RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:      repoRoot,
		Profile:       VerifyFailedTest,
		FailedTestLog: logPath,
		Stdout:        &bytes.Buffer{},
		Stderr:        &bytes.Buffer{},
	})
}

// Validates: R-6.10.25, R-6.10.26
//
// go test's deadline ends a seed before its cleanup can print the replay
// line, and the timeout panic lists the seed's subtest as running. The rerun
// is built from that name: the family's variable set to that seed and the
// family's test alone, never another seed's line and never the family's
// default seeds. The name does not say whether the run had the race detector,
// so the rerun has it: a rerun without it would pass on a failure only the
// detector produces and report it fixed.
func TestRunVerifyFailedTestRerunsATimedOutSeedFromItsSubtestName(t *testing.T) {
	t.Parallel()

	for name, other := range map[string]string{"no replay line": "", "another seed's replay line": testOtherCrashReplay} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			runner := &fakeRunner{runStdoutFunc: goTestReportsPass(testCrashFamily + "/seed=" + testTimedOutSeed)}
			require.NoError(t, rerunFailedTest(t, runner, seedDeadlineLog(other)))

			require.Len(t, runner.runCommands, 1)
			command := runner.runCommands[0]
			assert.Equal(t,
				[]string{"test", "-race", "-run=^" + testCrashFamily + "$", "-count=1", "-v", "-timeout=30m", "./internal/sync"},
				command.args)
			assertCommandHasEnvVar(t, command, seedsModelSeedEnv+"="+testTimedOutSeed)
			assertCommandHasEnvVar(t, command, seedsDispatchOrderEnv+"=")
			assertCommandLacksEnvVar(t, command, seedsModelSeedEnv+"=318012")
		})
	}
}

// Validates: R-6.10.26
//
// A rerun of a subtest that no longer exists runs its parent alone, which
// passes, and a seed's rerun that never reached the seed ran other seeds:
// neither reran the failure, so neither passes.
func TestRunVerifyFailedTestFailsWhenTheSubtestItAskedForDidNotRun(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		log     string
		printed string
		want    string
	}{
		"a subtest": {
			log:     "--- FAIL: TestWatchLoop/closed_scheduler (0.00s)\nFAIL\t" + repoModulePath + "/internal/sync\t0.1s\n",
			printed: "TestWatchLoop",
			want:    "TestWatchLoop/closed_scheduler",
		},
		"a seed": {log: seedDeadlineLog(""), printed: testCrashFamily + "/seed=1", want: testCrashFamily + "/seed=" + testTimedOutSeed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			runner := &fakeRunner{runStdoutFunc: goTestReportsPass(tc.printed)}
			err := rerunFailedTest(t, runner, tc.log)
			require.ErrorContains(t, err, "reported no result for "+tc.want)
			assert.Len(t, runner.runCommands, 1)
		})
	}
}

// Validates: R-6.10.26
//
// A rerun in which the asked-for test skipped ran nothing of what failed, and
// go test exits 0 on a skip: the rerun fails and says that the test skipped,
// in go test's verbose output and in its -json events alike.
func TestRunVerifyFailedTestFailsWhenTheTestItAskedForSkipped(t *testing.T) {
	t.Parallel()

	const failed = "TestWatchLoop/closed_scheduler"
	log := "--- FAIL: " + failed + " (0.00s)\nFAIL\t" + repoModulePath + "/internal/sync\t0.1s\n"
	outputs := map[string]string{
		"verbose output": "=== RUN   " + failed + "\n    --- SKIP: " + failed + " (0.00s)\n--- PASS: TestWatchLoop (0.00s)\nPASS\n",
		"json events": `{"Action":"skip","Package":"` + repoModulePath + `/internal/sync","Test":"` + failed + `"}` + "\n" +
			`{"Action":"pass","Package":"` + repoModulePath + `/internal/sync","Test":"TestWatchLoop"}` + "\n",
	}
	for name, output := range outputs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			runner := &fakeRunner{runStdoutFunc: func([]string) string { return output }}
			err := rerunFailedTest(t, runner, log)
			require.ErrorContains(t, err, "the rerun skipped "+failed)
			assert.Len(t, runner.runCommands, 1)
		})
	}
}

// Validates: R-6.10.25, R-6.10.26
//
// A failing subtest of a seeded family whose name holds no seed cannot be
// rerun as a seed, and a rerun of the family without one runs other seeds:
// the rerun refuses, naming the subtest, and runs nothing.
func TestRunVerifyFailedTestRefusesASeededFamilySubtestThatNamesNoSeed(t *testing.T) {
	t.Parallel()

	for _, subtest := range []string{"seed=x1", "seed=99999999999999999999", "batch=3"} {
		runner := &fakeRunner{}
		err := rerunFailedTest(t, runner,
			"    --- FAIL: "+testCrashFamily+"/"+subtest+" (1.00s)\nFAIL\t"+repoModulePath+"/internal/sync\t1.1s\n")
		require.ErrorContains(t, err, testCrashFamily+"/"+subtest)
		assert.Empty(t, runner.runCommands, subtest)
	}
}

// Validates: R-6.10.23, R-6.10.25
//
// A seed that go test's deadline ended printed no replay line, and the
// timeout panic lists its subtest as running: the night keys it by the seed
// its subtest names, beside any seed of the same test that printed a line.
func TestNightlyFailureKeysKeyATimedOutSeedByItsSubtestName(t *testing.T) {
	t.Parallel()

	timedOut := "internal/sync." + testCrashFamily + "/" + seedsModelSeedEnv + "=" + testTimedOutSeed
	other := "internal/sync." + testCrashFamily + "/" + seedsModelSeedEnv + "=318012"
	cases := map[string]struct {
		replay string
		want   map[string][]string
	}{
		"no replay line":             {want: map[string][]string{timedOut: {nightlySeedsJob}}},
		"another seed's replay line": {replay: testOtherCrashReplay, want: map[string][]string{other: {nightlySeedsJob}, timedOut: {nightlySeedsJob}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			remote := newClaimRemote(t, checkConclusionFailure)
			remote.jobs = nightlyRunView(t, map[string]string{nightlySeedsJob: checkConclusionFailure})
			remote.jobLogs[nightlyTestSeedsLog] = seedDeadlineLog(tc.replay)
			assert.Equal(t, tc.want, nightlyTestFailures(t, remote))
		})
	}
}

// Validates: R-6.10.25
//
// The reader takes only the variables devtool declares: a family's seed
// variable first, for that family's test, then at most the dispatch order.
// A line that assigns anything else, the toolchain, go's flags or an
// undeclared variable of the project, is no replay, so its words never reach
// a rerun's environment or a public issue.
func TestSeedReplaysFromLogTakesOnlyDeclaredVariables(t *testing.T) {
	t.Parallel()

	const engine = "go test -run '^TestRunOnce_RandomizedBidirectionalEditsConverge$' ./internal/sync"
	for _, refused := range []string{
		"GOTOOLCHAIN=go1.20 " + engine,
		"ONEDRIVE_GO_MODEL_SEED=3 GOTOOLCHAIN=go1.20 " + engine,
		"ONEDRIVE_GO_MODEL_SEED=3 GOFLAGS=-a " + engine,
		"ONEDRIVE_GO_MODEL_SEED=3 ONEDRIVE_GO_MODEL_BATCHES=2 " + engine,
		"ONEDRIVE_GO_PLANNER_SEED=3 " + engine,
		"ONEDRIVE_GO_MODEL_SEED=3 ONEDRIVE_GO_DISPATCH_ORDER=sideways " + engine,
		"ONEDRIVE_GO_MODEL_SEED=3 ONEDRIVE_GO_DISPATCH_ORDER=reverse ONEDRIVE_GO_DISPATCH_ORDER=forward " + engine,
		"ONEDRIVE_GO_MODEL_SEED=3 go test -run '^TestSomethingElse$' ./internal/sync",
		"ONEDRIVE_GO_MODEL_SEED=3 go test -run '^TestRunOnce_RandomizedBidirectionalEditsConverge$' ./internal/multisync",
	} {
		_, ok := parseSeedReplay(refused)
		assert.False(t, ok, refused)
	}
	for _, read := range []string{
		testEngineReplay,
		testPlannerReplay,
		"ONEDRIVE_GO_MODEL_SEED=3 go test -race -run '^TestModel_ChaosGraphConverges$' ./internal/sync",
		"ONEDRIVE_GO_MODEL_SEED=3 go test -run '^TestModel_CrashAtSeededPointsRecovers$' ./internal/sync",
		"ONEDRIVE_GO_MODEL_SEED=3 ONEDRIVE_GO_DISPATCH_ORDER=reverse " + engine,
		"ONEDRIVE_GO_MODEL_SEED=3 ONEDRIVE_GO_DISPATCH_ORDER=shuffle:7 " + engine,
	} {
		replay, ok := parseSeedReplay(read)
		require.True(t, ok, read)
		assert.Equal(t, read, replay.command())
	}
}
