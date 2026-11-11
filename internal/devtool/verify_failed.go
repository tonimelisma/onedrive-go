package devtool

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
)

const (
	repoModulePath         = "github.com/tonimelisma/onedrive-go"
	failedTestPackageE2E   = "./e2e/..."
	failedTestE2ETimeout   = "15m"
	rawGoTestFailurePrefix = "--- FAIL:"
	rawGoTestPackagePrefix = "FAIL"
	rawGoTestPassPrefix    = "ok"
	// goTestShufflePrefix starts the line a test binary run with -shuffle
	// prints first: `-test.shuffle <seed>`.
	goTestShufflePrefix = "-test.shuffle "
	goTestActionOutput  = "output"
)

type failedGoTestTarget struct {
	Package string
	Test    string
	// ShuffleSeed is the -shuffle seed go test printed for Package, empty
	// when the package ran its tests in source order.
	ShuffleSeed string
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
		target, err := lastFailedGoTestTarget(strings.NewReader(string(data)))
		if err != nil {
			return err
		}
		args, err := failedGoTestRerunArgs(target)
		if err != nil {
			return err
		}
		if err := writeStatus(stdout, "==> go "+strings.Join(args, " ")+"\n"); err != nil {
			return fmt.Errorf("write status: %w", err)
		}
		if err := runner.Run(ctx, repoRoot, env, stdout, stderr, "go", args...); err != nil {
			return fmt.Errorf("rerun failed test: %w", err)
		}
		return nil
	})
}

func lastFailedGoTestTarget(r io.Reader) (failedGoTestTarget, error) {
	scanner := bufio.NewScanner(r)
	scan := failedLogScan{seedByPackage: make(map[string]string)}
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			scan.observe(line)
		}
	}
	if err := scanner.Err(); err != nil {
		return failedGoTestTarget{}, fmt.Errorf("scan failed test log: %w", err)
	}

	last := scan.last
	if last.Package == "" && last.Test == "" {
		return failedGoTestTarget{}, fmt.Errorf("verify failed-test: no failed Go test found in log")
	}
	last.ShuffleSeed = scan.seedByPackage[normalizeFailedGoTestPackage(last.Package)]

	return last, nil
}

// failedLogScan walks a go test log, raw or -json, keeping the last failed
// target and the shuffle seed each package printed.
type failedLogScan struct {
	last          failedGoTestTarget
	seedByPackage map[string]string
	// pendingRawSeed is a raw seed line not yet followed by its package's
	// result line: raw output names the package only at the end.
	pendingRawSeed string
}

func (s *failedLogScan) observe(line string) {
	if pkg, seed, ok := shuffleSeedFromJSONLine(line); ok {
		s.seedByPackage[normalizeFailedGoTestPackage(pkg)] = seed
		return
	}
	if target, ok := failedGoTestTargetFromJSONLine(line); ok {
		s.last = mergeFailedGoTestTarget(s.last, target)
		return
	}
	if seed, ok := shuffleSeedFromOutput(line); ok {
		s.pendingRawSeed = seed
		return
	}
	if test, ok := failedGoTestNameFromRawLine(line); ok {
		s.last.Test = test
		return
	}
	if pkg, ok := failedGoTestPackageFromRawLine(line); ok {
		s.last.Package = pkg
		s.claimRawSeed(pkg)
		return
	}
	if pkg, ok := passedGoTestPackageFromRawLine(line); ok {
		s.claimRawSeed(pkg)
	}
}

// claimRawSeed gives the pending raw seed line to the package whose result
// line closes its output.
func (s *failedLogScan) claimRawSeed(pkg string) {
	if s.pendingRawSeed == "" {
		return
	}

	s.seedByPackage[normalizeFailedGoTestPackage(pkg)] = s.pendingRawSeed
	s.pendingRawSeed = ""
}

// shuffleSeedFromJSONLine reads the seed from a -json output event that
// carries the shuffle line.
func shuffleSeedFromJSONLine(line string) (string, string, bool) {
	start := strings.Index(line, "{")
	if start < 0 {
		return "", "", false
	}

	var event struct {
		Action  string
		Package string
		Output  string
	}
	if err := json.Unmarshal([]byte(line[start:]), &event); err != nil || event.Action != goTestActionOutput {
		return "", "", false
	}
	seed, ok := shuffleSeedFromOutput(event.Output)
	if !ok || event.Package == "" {
		return "", "", false
	}

	return event.Package, seed, true
}

// shuffleSeedFromOutput reads the seed from a `-test.shuffle <seed>` line. The
// seed must be an integer, because it goes back onto a command line.
func shuffleSeedFromOutput(output string) (string, bool) {
	seed, ok := strings.CutPrefix(strings.TrimSpace(output), goTestShufflePrefix)
	if !ok {
		return "", false
	}
	if _, err := strconv.ParseInt(seed, 10, 64); err != nil {
		return "", false
	}

	return seed, true
}

func mergeFailedGoTestTarget(last failedGoTestTarget, next failedGoTestTarget) failedGoTestTarget {
	if next.Test != "" {
		return next
	}
	if next.Package == "" {
		return last
	}
	if last.Test != "" && (last.Package == "" || normalizeFailedGoTestPackage(last.Package) == normalizeFailedGoTestPackage(next.Package)) {
		last.Package = next.Package
		return last
	}
	return next
}

func failedGoTestTargetFromJSONLine(line string) (failedGoTestTarget, bool) {
	start := strings.Index(line, "{")
	if start < 0 {
		return failedGoTestTarget{}, false
	}
	var event struct {
		Action  string
		Package string
		Test    string
	}
	if err := json.Unmarshal([]byte(line[start:]), &event); err != nil {
		return failedGoTestTarget{}, false
	}
	if event.Action != "fail" {
		return failedGoTestTarget{}, false
	}
	if event.Package == "" && event.Test == "" {
		return failedGoTestTarget{}, false
	}
	return failedGoTestTarget{Package: event.Package, Test: event.Test}, true
}

func failedGoTestNameFromRawLine(line string) (string, bool) {
	if !strings.HasPrefix(line, rawGoTestFailurePrefix) {
		return "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, rawGoTestFailurePrefix))
	if rest == "" {
		return "", false
	}
	name := strings.Fields(rest)[0]
	return strings.TrimSpace(name), name != ""
}

func failedGoTestPackageFromRawLine(line string) (string, bool) {
	if !strings.HasPrefix(line, rawGoTestPackagePrefix) {
		return "", false
	}
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != rawGoTestPackagePrefix {
		return "", false
	}
	return fields[1], fields[1] != ""
}

// passedGoTestPackageFromRawLine reads the package of an `ok <pkg> <time>`
// result line.
func passedGoTestPackageFromRawLine(line string) (string, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != rawGoTestPassPrefix {
		return "", false
	}

	return fields[1], true
}

// failedGoTestRerunArgs builds the rerun of one failed test. It keeps the
// race detector, which the full tier runs every test under, and the shuffle
// seed the failing package ran with: a rerun without either can pass on a
// failure only they produce and report a race or an order dependence fixed.
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
	args = append(args, "-race")
	if target.ShuffleSeed != "" {
		args = append(args, "-shuffle="+target.ShuffleSeed)
	}
	if target.Test != "" {
		args = append(args, "-run="+exactGoTestRunPattern(target.Test))
	}
	args = append(args, "-count=1", "-v")
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
