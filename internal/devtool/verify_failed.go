package devtool

import (
	"context"
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
		target, err := lastFailedGoTestTarget(string(data))
		if err != nil {
			return err
		}
		args, err := failedGoTestRerunArgs(target)
		if err != nil {
			return err
		}
		if statusErr := writeStatus(stdout, "==> go "+strings.Join(args, " ")+"\n"); statusErr != nil {
			return fmt.Errorf("write status: %w", statusErr)
		}
		// A test renamed or deleted since the failing run matches nothing,
		// and go test exits 0 on that: the rerun must fail instead of
		// reporting the failure fixed.
		if strings.HasSuffix(args[len(args)-1], "/...") {
			return runGoTestMatchingTree(ctx, runner, repoRoot, env, stdout, stderr, "rerun failed test", args...)
		}
		_, err = runGoTestMatching(ctx, runner, repoRoot, env, stdout, stderr, "rerun failed test", args...)

		return err
	})
}

// lastFailedGoTestTarget is the rerun target of the failure a go test log
// settled last, with the shuffle seed its package ran with.
func lastFailedGoTestTarget(log string) (failedGoTestTarget, error) {
	parsed := parseGoTestLog(log)
	last, ok := parsed.lastFailure()
	if !ok {
		return failedGoTestTarget{}, fmt.Errorf("verify failed-test: no failed Go test found in log")
	}

	return failedGoTestTarget{Package: last.pkg, Test: last.test, ShuffleSeed: parsed.shuffleSeed(last.pkg)}, nil
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
