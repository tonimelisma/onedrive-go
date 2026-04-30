package devtool

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
)

const (
	repoModulePath         = "github.com/tonimelisma/onedrive-go"
	failedTestPackageE2E   = "./e2e/..."
	failedTestE2ETimeout   = "15m"
	rawGoTestFailurePrefix = "--- FAIL:"
	rawGoTestPackagePrefix = "FAIL"
)

type failedGoTestTarget struct {
	Package string
	Test    string
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
	var last failedGoTestTarget
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if target, ok := failedGoTestTargetFromJSONLine(line); ok {
			last = mergeFailedGoTestTarget(last, target)
			continue
		}
		if test, ok := failedGoTestNameFromRawLine(line); ok {
			last.Test = test
			continue
		}
		if pkg, ok := failedGoTestPackageFromRawLine(line); ok {
			last.Package = pkg
		}
	}
	if err := scanner.Err(); err != nil {
		return failedGoTestTarget{}, fmt.Errorf("scan failed test log: %w", err)
	}
	if last.Package == "" && last.Test == "" {
		return failedGoTestTarget{}, fmt.Errorf("verify failed-test: no failed Go test found in log")
	}
	return last, nil
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

func failedGoTestRerunArgs(target failedGoTestTarget) ([]string, error) {
	pkg := normalizeFailedGoTestPackage(target.Package)
	if pkg == "" {
		return nil, fmt.Errorf("verify failed-test: failed test %q did not include a package", target.Test)
	}
	if isE2EPackage(pkg) || strings.HasPrefix(target.Test, "TestE2E_") {
		args := []string{"test", "-tags=e2e e2e_full", "-race"}
		if target.Test != "" {
			args = append(args, "-run="+exactGoTestRunPattern(target.Test))
		}
		args = append(args, "-count=1", "-v", "-parallel", "1", "-timeout="+failedTestE2ETimeout, failedTestPackageE2E)
		return args, nil
	}

	args := []string{"test"}
	if target.Test != "" {
		args = append(args, "-run="+exactGoTestRunPattern(target.Test))
	}
	args = append(args, "-count=1", "-v", pkg)
	return args, nil
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
