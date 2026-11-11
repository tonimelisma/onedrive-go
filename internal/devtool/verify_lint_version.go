package devtool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	ciWorkflowPath = ".github/workflows/ci.yml"

	lintBinaryName    = "golangci-lint"
	lintInstallTarget = "github.com/golangci/golangci-lint/v2/cmd/golangci-lint"

	// lintInstallDirFallback is where `go install` puts a binary when neither
	// GOBIN nor PATH says where the linter lives. The shell expands it.
	lintInstallDirFallback = "$(go env GOPATH)/bin"

	goBinEnvVar = "GOBIN"
	pathEnvVar  = "PATH"

	// anyExecuteBit is set on a file some user may run.
	anyExecuteBit os.FileMode = 0o111
)

var (
	// pinnedLintVersionPattern matches the version the CI workflow installs.
	pinnedLintVersionPattern = regexp.MustCompile(`golangci-lint-action@[^\n]*\n(?:.*\n)??\s*version:\s*v?(\d+\.\d+\.\d+)`)

	// installedLintVersionPattern matches `golangci-lint has version X.Y.Z ...`.
	installedLintVersionPattern = regexp.MustCompile(`golangci-lint has version v?(\d+\.\d+\.\d+)`)

	// lintBuildToolchainPattern matches the Go release the linter binary was
	// built with, as `golangci-lint --version` prints it: `built with go1.26.6`.
	lintBuildToolchainPattern = regexp.MustCompile(`built with go(\d+\.\d+)`)
)

// assertLintVersionMatchesCI fails when the installed golangci-lint is not the
// version CI pins, or was built with another Go release than go.mod pins.
//
// A different version is a different lint policy, not a cosmetic difference.
// Local verification then passes on rules CI does not have, or misses rules it
// does, and the disagreement only surfaces after a push. The pin is read from
// the workflow so the two cannot drift apart through a second copy of the
// number.
//
// A linter of the right version that was built with a newer Go than the pinned
// toolchain is as unusable: it cannot read the export data of the toolchain the
// repository builds with, and every typecheck fails. Both failures print the
// one command that installs a usable linter over the one that ran.
func assertLintVersionMatchesCI(
	ctx context.Context, runner commandRunner, repoRoot string, env []string,
) error {
	pinned, err := pinnedLintVersion(repoRoot)
	if err != nil {
		return err
	}

	// No workflow means no pin to disagree with. A workflow that exists but
	// does not pin is different, and pinnedLintVersion reports that: an
	// unpinned CI lint floats, which is the drift this check exists to stop.
	if pinned == "" {
		return nil
	}

	toolchain, err := pinnedGoToolchain(repoRoot)
	if err != nil {
		return err
	}

	out, runErr := runner.CombinedOutput(ctx, repoRoot, env, lintBinaryName, "--version")
	if runErr != nil {
		return fmt.Errorf("read golangci-lint version: %w", runErr)
	}

	m := installedLintVersionPattern.FindStringSubmatch(string(out))
	if m == nil {
		return fmt.Errorf("read golangci-lint version: unrecognized output %q", strings.TrimSpace(string(out)))
	}

	if m[1] != pinned {
		return fmt.Errorf(
			"golangci-lint %s is installed but %s pins v%s; "+
				"a different version is a different lint policy, so install the pinned one:\n  %s",
			m[1], ciWorkflowPath, pinned, lintInstallCommand(env, toolchain, pinned))
	}

	return assertLintBuiltWithPinnedGo(string(out), env, toolchain, pinned)
}

// assertLintBuiltWithPinnedGo compares the Go release the linter was built
// with against the pinned toolchain. Only the minor version decides whether
// the linter can read the toolchain's export data, so patch releases of one
// minor are interchangeable. A tree without a go.mod, and version output that
// names no build toolchain, leave nothing to compare.
func assertLintBuiltWithPinnedGo(versionOutput string, env []string, toolchain, lintVersion string) error {
	pinnedMinor := goMinorVersion(toolchain)
	if pinnedMinor == "" {
		return nil
	}

	m := lintBuildToolchainPattern.FindStringSubmatch(versionOutput)
	if len(m) <= 1 || m[1] == pinnedMinor {
		return nil
	}

	return fmt.Errorf(
		"golangci-lint v%s was built with go%s but %s pins %s; "+
			"a linter built with another Go release cannot typecheck this repository, so rebuild it:\n  %s",
		lintVersion, m[1], goModFileName, toolchain, lintInstallCommand(env, toolchain, lintVersion))
}

// lintInstallCommand returns the shell command that installs the pinned linter,
// built with the pinned toolchain, over the binary the check just ran.
func lintInstallCommand(env []string, toolchain, lintVersion string) string {
	install := "go install " + lintInstallTarget + "@v" + lintVersion
	if toolchain == "" {
		return install
	}

	return goToolchainEnvVar + "=" + toolchain + " " + goBinEnvVar + "=" + lintInstallDir(env) + " " + install
}

// lintInstallDir returns the directory the rebuild must install into: the PATH
// directory that holds the linter the check just ran, else GOBIN, else the go
// command's default. PATH comes first because installing anywhere else leaves
// the stale binary in front, and the check fails again.
func lintInstallDir(env []string) string {
	for _, dir := range filepath.SplitList(envValue(env, pathEnvVar)) {
		if dir == "" {
			continue
		}

		info, err := os.Stat(filepath.Join(dir, lintBinaryName))
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&anyExecuteBit != 0 {
			return dir
		}
	}

	if goBin := envValue(env, goBinEnvVar); goBin != "" {
		return goBin
	}

	return lintInstallDirFallback
}

func pinnedLintVersion(repoRoot string) (string, error) {
	path := filepath.Join(repoRoot, ciWorkflowPath)

	data, err := os.ReadFile(path) //nolint:gosec // repo-relative verifier input
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}

		return "", fmt.Errorf("read %s: %w", ciWorkflowPath, err)
	}

	m := pinnedLintVersionPattern.FindStringSubmatch(string(data))
	if m == nil {
		return "", fmt.Errorf("%s does not pin a golangci-lint version", ciWorkflowPath)
	}

	return m[1], nil
}
