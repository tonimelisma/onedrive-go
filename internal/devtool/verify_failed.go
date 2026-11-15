package devtool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
)

const (
	repoModulePath       = "github.com/tonimelisma/onedrive-go"
	failedTestPackageE2E = "./e2e/..."
	failedTestE2ETimeout = "15m"
	// failedTestSeedTimeout is the deadline of one seed's rerun.
	failedTestSeedTimeout = "30m"
	// rawGoTestSkipPrefix starts a verbose skipped test's result line.
	rawGoTestSkipPrefix = "--- SKIP:"
)

type failedGoTestTarget struct {
	Package string
	Test    string
	// ShuffleSeed is the -shuffle seed go test printed for Package, empty
	// when the package ran its tests in source order.
	ShuffleSeed string
	// Replay reruns the failure when it is a model test's seed: the replay
	// line the log holds for it, or the replay its subtest's name gives when
	// the test printed none. It is nil for any other failure.
	Replay *seedReplay
}

func runFailedTestVerification(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
	failedLogPath string,
) error {
	if failedLogPath == "" {
		return fmt.Errorf("verify failed-test: --failed-log is required")
	}

	return collector.runStep("failed test rerun", func() error {
		data, err := readFile(failedLogPath)
		if err != nil {
			return fmt.Errorf("read failed test log %s: %w", failedLogPath, err)
		}
		target, err := lastFailedGoTestTarget(string(data))
		if err != nil {
			return err
		}
		args, err := failedGoTestRerunArgs(target)
		if err != nil {
			return err
		}
		rerunEnv := env
		assignments := ""
		if target.Replay != nil {
			rerunEnv = append(append([]string{}, env...), target.Replay.rerunEnv()...)
			assignments = strings.Join(target.Replay.rerunEnv(), " ") + " "
		}
		if statusErr := writeStatus(stdout, "==> "+assignments+"go "+strings.Join(args, " ")+"\n"); statusErr != nil {
			return fmt.Errorf("write status: %w", statusErr)
		}
		// A test renamed or deleted since the failing run matches nothing,
		// and go test exits 0 on that: the rerun must fail instead of
		// reporting the failure fixed. A pattern can also match the parent of
		// a subtest that is gone, which then passes alone, so the rerun must
		// report the failed test itself.
		output, err := runGoTestMatching(ctx, runner, repoRoot, rerunEnv, stdout, stderr, "rerun failed test", args...)
		if ranErr := assertRerunReportsTest(output, target.rerunTest()); ranErr != nil {
			return errors.Join(ranErr, err)
		}

		return err
	})
}

// rerunTest is the full name of the test whose result the rerun must
// report: the seed's subtest for a seed, else the failed test, empty for a
// package that failed without naming one.
func (t failedGoTestTarget) rerunTest() string {
	if t.Replay != nil {
		return t.Replay.subtest()
	}

	return t.Test
}

// assertRerunReportsTest fails a rerun whose output reports no pass or fail
// of test: a parent whose subtest is gone runs alone and passes, a seed's
// rerun that never reached the seed ran other seeds, and a test that skipped
// ran nothing of what failed, so none of them reran the failure. An empty
// test asks for nothing.
func assertRerunReportsTest(output string, test string) error {
	if test == "" {
		return nil
	}
	skipped := false
	for line := range strings.Lines(output) {
		switch lineResultOfTest(line, test) {
		case rerunResultRan:
			return nil
		case rerunResultSkipped:
			skipped = true
		case rerunResultNone:
		}
	}
	if skipped {
		return fmt.Errorf("verify failed-test: the rerun skipped %s, so it did not rerun the failure", test)
	}

	return fmt.Errorf("verify failed-test: the rerun reported no result for %s, so it did not rerun the failure", test)
}

// rerunResult is what one line of a rerun's output says of the test the rerun
// asked for.
type rerunResult int

const (
	// rerunResultNone: the line is no result of the test.
	rerunResultNone rerunResult = iota
	// rerunResultRan: the test passed or failed.
	rerunResultRan
	// rerunResultSkipped: the test skipped.
	rerunResultSkipped
)

// lineResultOfTest reads one line of go test output as test's result: a
// verbose --- PASS, --- FAIL or --- SKIP line naming it, or a -json pass, fail
// or skip event of it.
func lineResultOfTest(line string, test string) rerunResult {
	text, event, isEvent := decodeGoTestLogLine(line)
	if isEvent {
		switch {
		case event.Test != test:
			return rerunResultNone
		case event.Action == goTestActionPass || event.Action == goTestActionFail:
			return rerunResultRan
		case event.Action == goTestActionSkip:
			return rerunResultSkipped
		default:
			return rerunResultNone
		}
	}
	trimmed := strings.TrimSpace(text)
	for prefix, result := range map[string]rerunResult{
		rawGoTestPassPrefix:    rerunResultRan,
		rawGoTestFailurePrefix: rerunResultRan,
		rawGoTestSkipPrefix:    rerunResultSkipped,
	} {
		if rest, found := strings.CutPrefix(trimmed, prefix); found {
			if fields := strings.Fields(rest); len(fields) > 0 && fields[0] == test {
				return result
			}
		}
	}

	return rerunResultNone
}

// lastFailedGoTestTarget is the rerun target of the failure a go test log
// settled last, with the shuffle seed its package ran with and, for a model
// test's failing seed, the replay that reruns it.
func lastFailedGoTestTarget(log string) (failedGoTestTarget, error) {
	parsed := parseGoTestLog(log)
	last, ok := parsed.lastFailure()
	if !ok {
		return failedGoTestTarget{}, fmt.Errorf("verify failed-test: no failed Go test found in log")
	}
	replay, err := replayOfFailure(seedReplaysFromLog(log), last)
	if err != nil {
		return failedGoTestTarget{}, err
	}

	return failedGoTestTarget{
		Package:     last.pkg,
		Test:        last.test,
		ShuffleSeed: parsed.shuffleSeed(last.pkg),
		Replay:      replay,
	}, nil
}

// replayOfFailure is the replay that reruns failure. For a seed's subtest it
// is the replay line of that seed, or, when the test printed none (go test's
// deadline ended it), the replay the subtest's name gives; another seed's
// line never stands in for it. For a family's top-level test it is the
// last replay line of the test. It is nil for any other failure, and an error
// for a family's subtest whose name holds no seed.
func replayOfFailure(replays []seedReplay, failure goTestFailure) (*seedReplay, error) {
	built, isSeed, err := seedReplayFromSubtest(failure)
	if err != nil {
		return nil, fmt.Errorf("verify failed-test: %w", err)
	}
	var last *seedReplay
	for i := range replays {
		if !replays[i].replays(failure) {
			continue
		}
		if isSeed && replays[i].sameSeed(built) {
			return &replays[i], nil
		}
		last = &replays[i]
	}
	if isSeed {
		return &built, nil
	}

	return last, nil
}

// failedGoTestRerunArgs builds the rerun of one failed test. It keeps the
// race detector, which the full tier runs every test under, and the shuffle
// seed the failing package ran with: a rerun without either can pass on a
// failure only they produce and report a race or an order dependence fixed.
// A failing seed is rerun as its replay line says, with the race detector
// exactly when the failing run had it (the seed's work can differ under it)
// and its top-level test alone, which the replay's seed variable narrows to
// the seed. A replay built from a subtest's name does not know whether its
// run had the race detector, so its rerun has it, as the rerun of any other
// failure does: a rerun without it can pass on a failure only it produces.
// A seed's rerun names its own deadline, since go test's default of ten
// minutes is not known to cover every seed under the race detector.
func failedGoTestRerunArgs(target failedGoTestTarget) ([]string, error) {
	pkg := normalizeFailedGoTestPackage(target.Package)
	if pkg == "" {
		return nil, fmt.Errorf("verify failed-test: failed test %q did not include a package", target.Test)
	}
	isE2E := isE2EPackage(pkg) || strings.HasPrefix(target.Test, "TestE2E_")

	args := []string{"test"}
	if isE2E {
		args = append(args, "-tags=e2e e2e_full")
	}
	if target.Replay == nil || target.Replay.race || target.Replay.fromSubtestName {
		args = append(args, "-race")
	}
	if target.ShuffleSeed != "" {
		args = append(args, "-shuffle="+target.ShuffleSeed)
	}
	switch {
	case target.Replay != nil:
		args = append(args, "-run="+exactGoTestRunPattern(target.Replay.test))
	case target.Test != "":
		args = append(args, "-run="+exactGoTestRunPattern(target.Test))
	}
	args = append(args, "-count=1", "-v")
	if target.Replay != nil {
		args = append(args, "-timeout="+failedTestSeedTimeout)
	}
	if isE2E {
		return append(args, "-parallel", "1", "-timeout="+failedTestE2ETimeout, failedTestPackageE2E), nil
	}

	return append(args, pkg), nil
}

func normalizeFailedGoTestPackage(pkg string) string {
	pkg = strings.TrimSpace(pkg)
	if pkg == "" {
		return ""
	}
	if pkg == repoModulePath {
		return "."
	}
	if strings.HasPrefix(pkg, repoModulePath+"/") {
		return "." + strings.TrimPrefix(pkg, repoModulePath)
	}
	return pkg
}

func isE2EPackage(pkg string) bool {
	clean := path.Clean(strings.TrimPrefix(pkg, "./"))
	return clean == "e2e" || strings.HasPrefix(clean, "e2e/")
}

func exactGoTestRunPattern(test string) string {
	parts := strings.Split(test, "/")
	for i := range parts {
		parts[i] = "^" + regexp.QuoteMeta(parts[i]) + "$"
	}
	return strings.Join(parts, "/")
}
