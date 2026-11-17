package devtool

import (
	"bytes"
	"context"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stressSyncPackagePath is the import path go test reports for the sync step.
const stressSyncPackagePath = repoModulePath + "/internal/sync"

// Validates: R-6.10.8
//
// The sync step's -run selects its declared members and nothing else. The
// names are anchored, so a member does not bring in a test whose name extends
// its own, a non-member stays out, and so does a new sync test whose name
// holds a word the step's tests once were picked by, until it is declared.
// The multisync step names no -run: it runs every test of its package.
func TestStressRepeatedStepsRunExactlyTheirMembers(t *testing.T) {
	t.Parallel()

	steps := stressRepeatedSteps()
	require.Len(t, steps, 2)
	multisyncArgs := steps[0].command(unitTestReferenceCores).args
	assert.NotContains(t, multisyncArgs, "-run", "the multisync step runs every test of its package")
	assert.Equal(t, stressMultisyncTarget, multisyncArgs[len(multisyncArgs)-1])

	step := &steps[1]
	args := step.command(unitTestReferenceCores).args
	runAt := slices.Index(args, "-run")
	require.Positive(t, runAt, "step %s names its members", step.name)
	require.Less(t, runAt+1, len(args))
	assert.Equal(t, "^("+strings.Join(step.members(), "|")+")$", args[runAt+1])
	assert.Equal(t, syncTestTree, args[len(args)-1])

	selector, err := regexp.Compile(args[runAt+1])
	require.NoError(t, err)
	require.NotEmpty(t, step.members())
	for _, member := range step.members() {
		assert.True(t, selector.MatchString(member), "step %s runs %s", step.name, member)
		assert.False(t, selector.MatchString(member+"Again"), "step %s runs no test whose name extends %s", step.name, member)
	}
	for i := range step.selection.packages {
		for _, nonMember := range step.selection.packages[i].nonMembers() {
			assert.False(t, selector.MatchString(nonMember.test), "step %s leaves out %s", step.name, nonMember.test)
		}
	}
	assert.False(t, selector.MatchString("TestInventedWidget_DrainsTheQueueOnShutdown"))
	assert.True(t, step.selection.placement.requires("TestInventedWidget_DrainsTheQueueOnShutdown"),
		"the sync step's rule asks for such a test to be declared")
}

// Validates: R-6.10.8, R-6.10.26
//
// A repeated step's go test deadline is what one pass of its members may cost
// on the cores, times the runs, times the margin, so a deadline is never a
// bare number: 25 and 75 minutes on a CI runner, and on 2 cores the
// small-host budgets halved, times the same runs and margin. The runs are one
// constant in the arguments and the status line, which names the sync step's
// declared members by their count instead of their pattern. Every deadline
// is a literal.
func TestStressRepeatedStepsDeadlinesFollowFromTheirPassBudget(t *testing.T) {
	t.Parallel()

	for cores, deadlines := range map[int][]string{
		4: {"25m0s", "1h15m0s"},
		2: {"56m15s", "7h30m0s"},
	} {
		steps := stressRepeatedSteps()
		require.Len(t, steps, len(deadlines))
		for i := range steps {
			step := &steps[i]
			where := string(step.name) + " on " + strconv.Itoa(cores) + " cores"
			want := time.Duration(float64(step.passBudgetFor(cores)) * stressRepeatRuns * stressRepeatMargin)
			assert.Equal(t, want, step.timeout(cores), where)

			command := step.command(cores)
			head := []string{"test", "-json", "-race", "-count=50", "-shuffle=on", "-timeout=" + deadlines[i]}
			require.GreaterOrEqual(t, len(command.args), len(head), where)
			assert.Equal(t, head, command.args[:len(head)], where)
			shown := "==> go " + strings.Join(head, " ")
			if step.selection != nil {
				shown += " -run <the " + strconv.Itoa(len(step.members())) + " declared members of " + string(step.name) + ">"
			}
			assert.Equal(t, shown+" "+step.target+"\n", command.statusLine, where)
		}
	}
	assert.Equal(t, 50, stressRepeatRuns)
	assert.Equal(t, 30*time.Second, stressMemberRunBudgetFor(4))
	assert.Equal(t, 90*time.Second, stressMemberRunBudgetFor(2))
	assert.Equal(t, 180*time.Second, stressMemberRunBudgetFor(1))
}

// stressMultisyncPackagePath is the import path go test reports for the
// multisync step.
const stressMultisyncPackagePath = repoModulePath + "/internal/multisync"

// stressRunEvents are the go test -json events of one run of pkg's test.
func stressRunEvents(pkg, test string, elapsed time.Duration) string {
	seconds := strconv.FormatFloat(elapsed.Seconds(), 'f', -1, 64)

	return `{"Action":"run","Package":"` + pkg + `","Test":"` + test + `"}` + "\n" +
		`{"Action":"pass","Package":"` + pkg + `","Test":"` + test + `","Elapsed":` + seconds + "}\n"
}

// Validates: R-6.10.8, R-6.10.26
//
// A member fails its step when the slowest run of the member itself or of one
// of its subtests is over the budget of one run, once the step's tests ran,
// with one line per member naming it, the test or subtest of that run, its
// time and the budget. A parallel subtest runs after its parent's own time is
// taken, so a parent that reports 10 ms bounds nothing: its subtest's run is
// what counts. A test that failed in the same step stays reported beside it.
// The multisync step declares no members, and each of its tests is bounded
// the same way: one over the budget fails the step that otherwise passed.
func TestRunVerifyStressFailsAStepWhoseMemberRunsOverItsBudget(t *testing.T) {
	t.Parallel()

	multisyncArgs := stressRepeatedSteps()[0].command(unitTestReferenceCores).args
	syncArgs := stressRepeatedSteps()[1].command(unitTestReferenceCores).args
	over := stressMemberRunBudget + 1500*time.Millisecond
	runner := &fakeRunner{
		runStdoutFunc: func(args []string) string {
			switch {
			case slices.Equal(args, multisyncArgs):
				return stressRunEvents(stressMultisyncPackagePath, "TestInventedRunner_Stalls", over) +
					stressRunEvents(stressMultisyncPackagePath, "TestInventedRunner_Quick", time.Second)
			case !slices.Equal(args, syncArgs):
				return passingGoTestOutput(args)
			}

			return stressRunEvents(stressSyncPackagePath, "TestInventedMember_Spins", time.Second) +
				stressRunEvents(stressSyncPackagePath, "TestInventedMember_Spins/one_case", over) +
				stressRunEvents(stressSyncPackagePath, "TestInventedMember_Spins", over) +
				stressRunEvents(stressSyncPackagePath, "TestInventedMember_Quick", stressMemberRunBudget) +
				stressRunEvents(stressSyncPackagePath, "TestInventedMember_Fans/one_lane", over) +
				stressRunEvents(stressSyncPackagePath, "TestInventedMember_Fans/other_lane", time.Second) +
				stressRunEvents(stressSyncPackagePath, "TestInventedMember_Fans", 10*time.Millisecond)
		},
		runErrByKey: map[string]error{goCommandKey(syncArgs): assert.AnError},
	}
	summaryPath := filepath.Join(t.TempDir(), "stress-summary.json")

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:        t.TempDir(),
		Profile:         VerifyStress,
		Cores:           unitTestReferenceCores,
		SummaryJSONPath: summaryPath,
		Stdout:          &bytes.Buffer{},
		Stderr:          &bytes.Buffer{},
	})

	require.ErrorIs(t, err, assert.AnError, "the failed go test stays reported")
	var budgetLines []string
	for line := range strings.Lines(err.Error()) {
		if strings.Contains(line, "budget") {
			budgetLines = append(budgetLines, strings.TrimSpace(line))
		}
	}
	member := string(stressSyncConcurrencyRaceStepName) + ": member " + stressSyncPackagePath
	assert.Equal(t, []string{
		string(stressMultisyncRaceStepName) + ": member " + stressMultisyncPackagePath +
			" TestInventedRunner_Stalls took 31.5s in its slowest run, over the 30s budget of one run",
		member + " TestInventedMember_Fans took 31.5s in its slowest run, of subtest TestInventedMember_Fans/one_lane, " +
			"over the 30s budget of one run",
		member + " TestInventedMember_Spins took 31.5s in its slowest run, over the 30s budget of one run",
	}, budgetLines)

	var summary VerifySummary
	readVerifySummaryFile(t, summaryPath, &summary)
	step := requireVerifySummaryStep(t, summary, stressSyncConcurrencyRaceStepName)
	assert.Equal(t, verifySummaryStatusFail, step.Status)
	assert.Equal(t, verifySummaryStatusFail, requireVerifySummaryStep(t, summary, stressMultisyncRaceStepName).Status,
		"the multisync step fails on its test's run alone")
}

// Validates: R-6.10.8
//
// A stress step runs through go test -json, but what reaches the log is text:
// a failed test's output, and nothing of a test that passed, so the failure a
// person looks for is not one escaped event among the events of every passing
// run. The summary names each failed test of the step.
func TestRunVerifyStressLogsTextAndNamesTheFailedTests(t *testing.T) {
	t.Parallel()

	syncArgs := stressRepeatedSteps()[1].command(unitTestReferenceCores).args
	event := func(action, test, output string) string {
		return goTestEvent(stressSyncPackagePath, action, test, output)
	}
	failLine := rawGoTestPackageFailPrefix + stressSyncPackagePath + "\t0.02s\n"
	runner := &fakeRunner{
		runStdoutFunc: func(args []string) string {
			if !slices.Equal(args, syncArgs) {
				return passingGoTestOutput(args)
			}

			return event(goTestActionRun, "TestInventedMember_Holds", "") +
				event(goTestActionOutput, "TestInventedMember_Holds", "    holds_test.go:8: the holder chatters\n") +
				event(goTestActionPass, "TestInventedMember_Holds", "") +
				event(goTestActionRun, "TestInventedMember_Breaks", "") +
				event(goTestActionOutput, "TestInventedMember_Breaks", "    breaks_test.go:12: the gizmo broke\n") +
				event(goTestActionOutput, "TestInventedMember_Breaks", "--- FAIL: TestInventedMember_Breaks (0.00s)\n") +
				event(goTestActionFail, "TestInventedMember_Breaks", "") +
				event(goTestActionOutput, "", failLine) +
				event(goTestActionFail, "", "")
		},
		runErrByKey: map[string]error{goCommandKey(syncArgs): assert.AnError},
	}
	summaryPath := filepath.Join(t.TempDir(), "stress-summary.json")
	stdout := &bytes.Buffer{}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:        t.TempDir(),
		Profile:         VerifyStress,
		Cores:           unitTestReferenceCores,
		SummaryJSONPath: summaryPath,
		Stdout:          stdout,
		Stderr:          &bytes.Buffer{},
	})

	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, stdout.String(), "    breaks_test.go:12: the gizmo broke\n--- FAIL: TestInventedMember_Breaks (0.00s)\n")
	assert.Contains(t, stdout.String(), failLine)
	assert.NotContains(t, stdout.String(), "the holder chatters", "a passing test's output stays out of the log")
	assert.NotContains(t, stdout.String(), `"Action"`, "the log holds no events")

	var summary VerifySummary
	readVerifySummaryFile(t, summaryPath, &summary)
	step := requireVerifySummaryStep(t, summary, stressSyncConcurrencyRaceStepName)
	assert.Equal(t, verifySummaryStatusFail, step.Status)
	assert.Equal(t, []string{"TestInventedMember_Breaks"}, step.FailedTests)
}
