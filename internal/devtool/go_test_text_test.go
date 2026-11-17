package devtool

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gizmoPackage is the invented package of the event streams below, and
// widgetPackage a second one, for a go test of two packages.
const (
	gizmoPackage  = "example.com/inventory/gizmo"
	widgetPackage = "example.com/inventory/widget"
)

// raceDetectedInTest is the line the testing package prints for a test that
// was running when the race detector reported a race, and raceDetectedOutside
// the line it prints when no test failed for one.
const (
	raceDetectedInTest  = "    testing.go:1712: race detected during execution of test\n"
	raceDetectedOutside = "testing: race detected outside of test execution\n"
)

// pipeChunkBytes is how many bytes of a stream reach the writer at a time,
// fewer than a line, as a pipe may deliver them.
const pipeChunkBytes = 7

// gizmoCutLine is the last line a run cut off before its package ended
// printed.
const gizmoCutLine = "    gizmo_test.go:10: before the cut\n"

// goTestEvent is one `go test -json` event of pkg, as one line; an empty test
// is the package's own event, and an empty output carries none.
func goTestEvent(pkg, action, test, output string) string {
	fields := fmt.Sprintf(`"Action":%q,"Package":%q`, action, pkg)
	if test != "" {
		fields += fmt.Sprintf(`,"Test":%q`, test)
	}

	return fmt.Sprintf("{%s%s}\n", fields, goTestEventOutputField(output))
}

// goTestEventOutputField is an event's Output field, empty for no output.
func goTestEventOutputField(output string) string {
	if output == "" {
		return ""
	}

	return fmt.Sprintf(`,"Output":%q`, output)
}

// gizmoEvent is one event of gizmoPackage.
func gizmoEvent(action, test, output string) string {
	return goTestEvent(gizmoPackage, action, test, output)
}

// testRunEvents is the events of one run of pkg's test: its start, its output
// lines, and result (pass, fail or skip), or no result for a run that never
// ended.
func testRunEvents(pkg, test, result string, output ...string) string {
	events := goTestEvent(pkg, goTestActionRun, test, "") + goTestOutput(pkg, test, fmt.Sprintf("=== RUN   %s\n", test))
	events += goTestOutput(pkg, test, output...)
	if result != "" {
		events += goTestEvent(pkg, result, test, "")
	}

	return events
}

// goTestOutput is one output event of pkg's test for each line.
func goTestOutput(pkg, test string, lines ...string) string {
	var events strings.Builder
	for _, line := range lines {
		events.WriteString(goTestEvent(pkg, goTestActionOutput, test, line))
	}

	return events.String()
}

// goTestResultLine is go test's result line of pkg after prefix.
func goTestResultLine(pkg, prefix string) string {
	return fmt.Sprintf("%s%s\t0.01s\n", prefix, pkg)
}

// goTestEnd is the end of pkg: go test's result line, then the package's
// result action.
func goTestEnd(pkg, result, resultLine string) string {
	return goTestEvent(pkg, goTestActionOutput, "", resultLine) + goTestEvent(pkg, result, "", "")
}

// gizmoRun, gizmoOutput, gizmoResultLine and gizmoEnd are the four above for
// gizmoPackage.
func gizmoRun(test, result string, output ...string) string {
	return testRunEvents(gizmoPackage, test, result, output...)
}

func gizmoOutput(test string, lines ...string) string {
	return goTestOutput(gizmoPackage, test, lines...)
}

func gizmoResultLine(prefix string) string {
	return goTestResultLine(gizmoPackage, prefix)
}

func gizmoEnd(result, resultLine string) string {
	return goTestEnd(gizmoPackage, result, resultLine)
}

// gizmoParallel is the start of a parallel test of gizmoPackage, as
// test2json reports it: its run, then its pause, the PAUSE line before the
// event. gizmoContinue is the test going on: the event before the CONT line.
func gizmoParallel(test string) string {
	return gizmoEvent(goTestActionRun, test, "") +
		gizmoOutput(test, "=== RUN   "+test+"\n", "=== PAUSE "+test+"\n") + gizmoEvent(goTestActionPause, test, "")
}

func gizmoContinue(test string) string {
	return gizmoEvent(goTestActionCont, test, "") + gizmoOutput(test, "=== CONT  "+test+"\n")
}

// gizmoRaceReport is one data race report, line by line, as the race
// detector prints it.
func gizmoRaceReport() []string {
	return []string{
		"==================\n",
		"WARNING: DATA RACE\n",
		"Write at 0x00c000012345 by goroutine 8:\n",
		"  example.com/inventory/gizmo.TestGizmo_Races()\n",
		"      /src/inventory/gizmo/gizmo_test.go:14 +0x48\n",
		"\n",
		"Previous write at 0x00c000012345 by goroutine 10:\n",
		"  example.com/inventory/gizmo.TestGizmo_Races.func1()\n",
		"      /src/inventory/gizmo/gizmo_test.go:12 +0x30\n",
		"\n",
		"Goroutine 10 (finished) created at:\n",
		"  example.com/inventory/gizmo.TestGizmo_Races()\n",
		"      /src/inventory/gizmo/gizmo_test.go:12 +0x26\n",
		"==================\n",
	}
}

// gizmoTimeoutPanic is the panic a test binary prints when its -timeout fires
// while TestGizmo_Hangs runs, with the start of its goroutine dump.
func gizmoTimeoutPanic() []string {
	return []string{
		"panic: test timed out after 2s\n",
		"\trunning tests:\n",
		"\t\tTestGizmo_Hangs (2s)\n",
		"\n",
		"goroutine 7 [running]:\n",
		"testing.(*M).startAlarm.func1()\n",
		"\t/usr/lib/go/src/testing/testing.go:2802 +0x354\n",
		"created by time.goFunc\n",
		"\t/usr/lib/go/src/time/sleep.go:215 +0x2d\n",
	}
}

// gizmoTestBinary is the import path go test gives the package's test binary.
func gizmoTestBinary() string {
	return fmt.Sprintf("%s [%s.test]", gizmoPackage, gizmoPackage)
}

// gizmoBuildEvent is a build event of the package's test binary, which go
// test names by import path instead of package.
func gizmoBuildEvent(action, output string) string {
	return fmt.Sprintf("{\"ImportPath\":%q,\"Action\":%q%s}\n", gizmoTestBinary(), action, goTestEventOutputField(output))
}

// writeGoTestText is what the text writer makes of events, fed to it in
// pieces that split lines.
func writeGoTestText(t *testing.T, events string) string {
	t.Helper()

	var text bytes.Buffer
	writer := newGoTestTextWriter(&text)
	for chunk := range slices.Chunk([]byte(events), pipeChunkBytes) {
		_, err := writer.Write(chunk)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Flush())

	return text.String()
}

// goTestTextCase is one way a run fails: its events, a piece of text the log
// must hold whole, and, when set, text it must not hold.
type goTestTextCase struct {
	events  string
	written string
	dropped string
}

// Validates: R-6.10.8
//
// The stress lane's log holds the text go test prints without -v, not its
// -json events, and whatever reads the log reads the same in it: for each way
// a run fails, the log reader finds the same failures, deadline and shuffle
// seeds in the text as in the events, and the empty-run guard gives the same
// verdict. Each case's text holds whole the lines a person looks for: a race
// report with its stacks, a timeout's panic with its running tests and
// goroutines, a build failure with its package header.
func TestGoTestTextWriterKeepsWhatTheLogReaderFinds(t *testing.T) {
	t.Parallel()

	cases := goTestTextSingleTestCases()
	maps.Copy(cases, goTestTextParallelCases())
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			text := writeGoTestText(t, tc.events)

			assert.Contains(t, text, tc.written)
			if tc.dropped != "" {
				assert.NotContains(t, text, tc.dropped)
			}
			assert.NotContains(t, text, `"Action"`, "the text holds no events")
			assert.Equal(t, parseGoTestLog(tc.events), parseGoTestLog(text), "the log reader finds the same in both")
			rawGuard, textGuard := assertTestsMatched(tc.events, name), assertTestsMatched(text, name)
			assert.Equal(t, rawGuard == nil, textGuard == nil, "the empty-run guard reads both alike")
		})
	}
}

// goTestTextSingleTestCases are the ways one test fails a run of one
// package, the decisive text credited to that test or to the package.
func goTestTextSingleTestCases() map[string]goTestTextCase {
	failLine := gizmoResultLine(rawGoTestPackageFailPrefix)
	noTestsLine := fmt.Sprintf("%s%s\t0.01s [%s]\n", rawGoTestPackagePassPrefix, gizmoPackage, noTestsRanMarker)
	seedLine := gizmoEvent(goTestActionOutput, "", goTestShufflePrefix+"41\n")
	race := strings.Join(gizmoRaceReport(), "")
	timeout := strings.Join(gizmoTimeoutPanic(), "")
	buildHeader := "# " + gizmoTestBinary() + "\n"
	buildError := "gizmo_test.go:6:14: cannot use \"text\" as int value\n"

	return map[string]goTestTextCase{
		"a failing test": {
			events: seedLine +
				gizmoRun("TestGizmo_Breaks", goTestActionFail,
					"    gizmo_test.go:14: the gizmo broke\n", "--- FAIL: TestGizmo_Breaks (0.00s)\n") +
				gizmoEvent(goTestActionOutput, "", "FAIL\n") + gizmoEnd(goTestActionFail, failLine),
			written: "    gizmo_test.go:14: the gizmo broke\n--- FAIL: TestGizmo_Breaks (0.00s)\nFAIL\n" + failLine,
		},
		"a data race in the test it fails": {
			events: gizmoRun("TestGizmo_Races", goTestActionFail,
				append(gizmoRaceReport(), raceDetectedInTest, "--- FAIL: TestGizmo_Races (0.00s)\n")...) +
				gizmoEnd(goTestActionFail, failLine),
			written: race + raceDetectedInTest + "--- FAIL: TestGizmo_Races (0.00s)\n" + failLine,
		},
		"a panic": {
			events: gizmoRun("TestGizmo_Explodes", goTestActionFail,
				"--- FAIL: TestGizmo_Explodes (0.00s)\n", "panic: the gizmo exploded [recovered, repanicked]\n",
				"goroutine 7 [running]:\n") +
				gizmoEnd(goTestActionFail, failLine),
			written: "panic: the gizmo exploded [recovered, repanicked]\ngoroutine 7 [running]:\n" + failLine,
		},
		"a deadline in the test that hangs": {
			events: gizmoRun("TestGizmo_Holds", goTestActionPass, "--- PASS: TestGizmo_Holds (0.00s)\n") +
				gizmoRun("TestGizmo_Hangs", "", append([]string{"    gizmo_test.go:10: before the hang\n"},
					gizmoTimeoutPanic()...)...) +
				gizmoEnd(goTestActionFail, failLine),
			written: "    gizmo_test.go:10: before the hang\n" + timeout + failLine,
		},
		"a build failure": {
			events: gizmoBuildEvent(goTestActionBuildOutput, buildHeader) +
				gizmoBuildEvent(goTestActionBuildOutput, buildError) +
				gizmoBuildEvent("build-fail", "") +
				gizmoEvent("start", "", "") +
				gizmoEnd(goTestActionFail, rawGoTestPackageFailPrefix+gizmoPackage+rawGoTestBuildFailedSuffix+"\n"),
			written: buildHeader + buildError,
		},
		"no test ran": {
			events: seedLine +
				gizmoEvent(goTestActionOutput, "", "testing: warning: "+noTestsRanMarker+"\n") +
				gizmoEnd(goTestActionPass, noTestsLine),
			written: noTestsLine,
		},
		"a run cut off before its package ended": {
			events:  gizmoRun("TestGizmo_Hangs", "", gizmoCutLine),
			written: gizmoCutLine,
		},
	}
}

// goTestTextParallelCases are the ways a run fails while tests run in
// parallel, and a line another test's code logs, in the shapes test2json
// reports them. test2json credits a line without a framing marker, such as a
// race report, a timeout's panic or a raw log line, to the test the last
// framing line named: often a test that has printed its own --- PASS line
// and passes, and from a parallel test's PAUSE line to the next framing line
// one that is paused. A go test of two packages interleaves their events.
func goTestTextParallelCases() map[string]goTestTextCase {
	failLine := gizmoResultLine(rawGoTestPackageFailPrefix)
	passLine := gizmoResultLine(rawGoTestPackagePassPrefix)
	orchestratorLine := "2026/11/16 10:00:00 INFO the gizmo orchestrator started\n"
	race := strings.Join(gizmoRaceReport(), "")
	widgetOK := goTestResultLine(widgetPackage, rawGoTestPackagePassPrefix)
	widgetFail := goTestResultLine(widgetPackage, rawGoTestPackageFailPrefix)
	widgetBreaks := []string{"    widget_test.go:8: the widget broke\n", "--- FAIL: TestWidget_Breaks (0.00s)\n"}

	return map[string]goTestTextCase{
		"a deadline credited to a test that passed": {
			events: gizmoParallel("TestGizmo_Holds") + gizmoParallel("TestGizmo_Hangs") +
				gizmoContinue("TestGizmo_Holds") + gizmoContinue("TestGizmo_Hangs") +
				gizmoOutput("TestGizmo_Holds", "--- PASS: TestGizmo_Holds (0.10s)\n") +
				gizmoOutput("TestGizmo_Holds", gizmoTimeoutPanic()...) +
				gizmoEvent(goTestActionPass, "TestGizmo_Holds", "") + gizmoEnd(goTestActionFail, failLine),
			written: strings.Join(gizmoTimeoutPanic(), "") +
				"=== RUN   TestGizmo_Hangs\n=== PAUSE TestGizmo_Hangs\n=== CONT  TestGizmo_Hangs\n" + failLine,
		},
		"a race credited to a test that passed": {
			events: gizmoParallel("TestGizmo_Races") + gizmoParallel("TestGizmo_Quick") +
				gizmoContinue("TestGizmo_Quick") + gizmoContinue("TestGizmo_Races") +
				gizmoOutput("TestGizmo_Quick", "--- PASS: TestGizmo_Quick (0.05s)\n") +
				gizmoOutput("TestGizmo_Quick", gizmoRaceReport()...) + gizmoEvent(goTestActionPass, "TestGizmo_Quick", "") +
				gizmoOutput("TestGizmo_Races", raceDetectedInTest, "--- FAIL: TestGizmo_Races (0.21s)\n") +
				gizmoEvent(goTestActionFail, "TestGizmo_Races", "") +
				gizmoEvent(goTestActionOutput, "", "FAIL\n") + gizmoEnd(goTestActionFail, failLine),
			written: race + "=== RUN   TestGizmo_Races\n=== PAUSE TestGizmo_Races\n=== CONT  TestGizmo_Races\n" +
				raceDetectedInTest + "--- FAIL: TestGizmo_Races (0.21s)\nFAIL\n" + failLine,
		},
		"a line credited to a paused test": {
			events: gizmoParallel("TestGizmo_Waits") + gizmoOutput("TestGizmo_Waits", orchestratorLine) +
				gizmoContinue("TestGizmo_Waits") + gizmoOutput("TestGizmo_Waits", "--- PASS: TestGizmo_Waits (0.00s)\n") +
				gizmoEvent(goTestActionPass, "TestGizmo_Waits", "") +
				gizmoEvent(goTestActionOutput, "", "PASS\n") + gizmoEnd(goTestActionPass, passLine),
			written: orchestratorLine + "PASS\n" + passLine,
		},
		"a race held by a test that passes": {
			events: gizmoParallel("TestGizmo_Resumes") + gizmoContinue("TestGizmo_Resumes") +
				gizmoOutput("TestGizmo_Resumes", "    gizmo_test.go:9: the resumer chatters\n") +
				gizmoOutput("TestGizmo_Resumes", gizmoRaceReport()...) +
				gizmoOutput("TestGizmo_Resumes", "--- PASS: TestGizmo_Resumes (0.00s)\n", raceDetectedOutside) +
				gizmoEvent(goTestActionPass, "TestGizmo_Resumes", "") +
				gizmoEvent(goTestActionOutput, "", "FAIL\n") + gizmoEnd(goTestActionFail, failLine),
			written: race + raceDetectedOutside + "FAIL\n" + failLine,
			dropped: "the resumer chatters",
		},
		"two packages whose events interleave": {
			events: gizmoEvent(goTestActionOutput, "", goTestShufflePrefix+"41\n") +
				goTestOutput(widgetPackage, "", goTestShufflePrefix+"42\n") +
				gizmoRun("TestGizmo_Breaks", goTestActionFail,
					"    gizmo_test.go:14: the gizmo broke\n", "--- FAIL: TestGizmo_Breaks (0.00s)\n") +
				testRunEvents(widgetPackage, "TestWidget_Holds", goTestActionPass, "--- PASS: TestWidget_Holds (0.00s)\n") +
				goTestOutput(widgetPackage, "", "PASS\n") + goTestEnd(widgetPackage, goTestActionPass, widgetOK) +
				gizmoEvent(goTestActionOutput, "", "FAIL\n") + gizmoEnd(goTestActionFail, failLine),
			written: "--- FAIL: TestGizmo_Breaks (0.00s)\nFAIL\n" + failLine + goTestShufflePrefix + "42\nPASS\n" + widgetOK,
		},
		"two packages, one cut off": {
			events: gizmoRun("TestGizmo_Hangs", "", gizmoCutLine) +
				testRunEvents(widgetPackage, "TestWidget_Breaks", goTestActionFail, widgetBreaks...) +
				goTestOutput(widgetPackage, "", "FAIL\n") + goTestEnd(widgetPackage, goTestActionFail, widgetFail),
			written: "=== RUN   TestGizmo_Hangs\n" + gizmoCutLine + "=== RUN   TestWidget_Breaks\n" +
				strings.Join(widgetBreaks, "") + "FAIL\n" + widgetFail,
		},
	}
}

// Validates: R-6.10.8
//
// A test that passed or skipped prints nothing, as go test without -v prints
// nothing for it, and neither does a passing subtest of a test that failed:
// the package's own lines are all a passing run leaves in the log.
func TestGoTestTextWriterLeavesOutWhatPassed(t *testing.T) {
	t.Parallel()

	passLine := gizmoResultLine(rawGoTestPackagePassPrefix)
	passing := gizmoRun("TestGizmo_Holds", goTestActionPass,
		"    gizmo_test.go:9: the holder chatters\n", "--- PASS: TestGizmo_Holds (0.00s)\n") +
		gizmoRun("TestGizmo_Skips", goTestActionSkip, "    gizmo_test.go:20: skipped on this host\n") +
		gizmoEvent(goTestActionOutput, "", "PASS\n") + gizmoEnd(goTestActionPass, passLine)
	assert.Equal(t, "PASS\n"+passLine, writeGoTestText(t, passing))

	parent := gizmoRun("TestGizmo_Lanes", "") +
		gizmoRun("TestGizmo_Lanes/good", goTestActionPass, "    gizmo_test.go:31: the good lane chatters\n") +
		gizmoRun("TestGizmo_Lanes/bad", goTestActionFail, "    gizmo_test.go:32: the bad lane broke\n") +
		gizmoEvent(goTestActionOutput, "TestGizmo_Lanes", "--- FAIL: TestGizmo_Lanes (0.00s)\n") +
		gizmoEvent(goTestActionFail, "TestGizmo_Lanes", "")
	text := writeGoTestText(t, parent)
	assert.Contains(t, text, "the bad lane broke")
	assert.Contains(t, text, "--- FAIL: TestGizmo_Lanes (0.00s)")
	assert.NotContains(t, text, "the good lane chatters")
}

// errGizmoLogClosed is the failure of a log that no longer takes writes.
var errGizmoLogClosed = errors.New("the log is closed")

// closedLog is a log that refuses every write.
type closedLog struct{}

func (closedLog) Write([]byte) (int, error) {
	return 0, errGizmoLogClosed
}

// Validates: R-6.10.8
//
// A log that refuses a write fails the write of the stream that reached it,
// and the flush of what a run cut off left, so a step does not go on as if
// its output had been kept.
func TestGoTestTextWriterReturnsTheLogsFailure(t *testing.T) {
	t.Parallel()

	writer := newGoTestTextWriter(closedLog{})
	_, err := writer.Write([]byte(gizmoEnd(goTestActionPass, gizmoResultLine(rawGoTestPackagePassPrefix))))
	require.ErrorIs(t, err, errGizmoLogClosed)

	writer = newGoTestTextWriter(closedLog{})
	_, err = writer.Write([]byte(gizmoRun("TestGizmo_Hangs", "", gizmoCutLine)))
	require.NoError(t, err, "a running test's output waits for the test to end")
	require.ErrorIs(t, writer.Flush(), errGizmoLogClosed)
}
