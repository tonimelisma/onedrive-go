package devtool

import (
	"context"
	"errors"
	"fmt"
	"go/build"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// golangci-lint type-checks one build of the tree: one GOOS and one set of
// build tags. A file constrained to another platform or to a tag the build
// does not set is never read, by any rule, so the lint step runs one pass
// per build the tree needs read, and before them a source check fails on a
// Go file no pass builds, so a new constraint cannot go unread.

// lintPass is one golangci-lint run over the tree: the platform it
// type-checks for and the build tags it sets beyond .golangci.yml's.
type lintPass struct {
	// name is the pass as --lint-pass and the CI matrix name it.
	name string
	goos string
	// extraTags are added to lintConfigBuildTags for this pass. A pass
	// without any reads the configuration's tags alone.
	extraTags []string
}

// lintPassArch is the architecture every pass type-checks for. No file of
// the tree is constrained by architecture (TestEveryGoFileIsBuiltByALintPass
// would name one), so one serves every platform.
const lintPassArch = "amd64"

// lintPasses are the passes the lint step runs, in order. Linux is the
// primary platform and the CI runner's. darwin is the other primary platform
// and also carries the race tag: no file is constrained both ways, so one
// pass reads the darwin-only files and the race-only ones for the cost of
// one run, and the !race files are read by the linux pass. freebsd is the
// third platform README.md claims and the cross build builds, and the only
// build of the files for a unix that is neither linux nor darwin.
func lintPasses() []lintPass {
	return []lintPass{
		{name: "linux", goos: "linux"},
		{name: "darwin-race", goos: "darwin", extraTags: []string{"race"}},
		{name: "freebsd", goos: "freebsd"},
	}
}

// lintConfigBuildTags are the build tags of .golangci.yml's run.build-tags,
// which a pass with extra tags repeats on its command line, since golangci-lint's
// --build-tags replaces the configuration's list instead of adding to it.
// TestGolangciConfig_BuildTagsAreDevtoolsLintTags keeps the two equal.
func lintConfigBuildTags() []string {
	return []string{"e2e", "e2e_full", "integration"}
}

// buildTags are every build tag the pass sets.
func (p lintPass) buildTags() []string {
	return append(slices.Clone(lintConfigBuildTags()), p.extraTags...)
}

// commandArgs are golangci-lint's arguments for the pass after base, the
// arguments every pass shares: the tags flag only when the pass adds tags.
func (p lintPass) commandArgs(base []string) []string {
	args := slices.Clone(base)
	if len(p.extraTags) > 0 {
		args = append(args, "--build-tags="+strings.Join(p.buildTags(), ","))
	}

	return args
}

// env is the environment of the pass: the step's, with the pass's platform.
func (p lintPass) env(base []string) []string {
	return append(slices.Clone(base), "GOOS="+p.goos, "GOARCH="+lintPassArch)
}

// buildsFile reports whether the pass type-checks the Go file name in dir.
func (p lintPass) buildsFile(dir string, name string) (bool, error) {
	ctx := build.Default
	ctx.GOOS = p.goos
	ctx.GOARCH = lintPassArch
	ctx.CgoEnabled = false
	ctx.BuildTags = p.buildTags()
	matched, err := ctx.MatchFile(dir, name)
	if err != nil {
		return false, fmt.Errorf("match %s for lint pass %s: %w", filepath.Join(dir, name), p.name, err)
	}

	return matched, nil
}

// resolveLintPasses is the pass --lint-pass names, or every pass when it
// names none.
func resolveLintPasses(name string) ([]lintPass, error) {
	if name == "" {
		return lintPasses(), nil
	}
	names := []string{}
	for _, pass := range lintPasses() {
		if pass.name == name {
			return []lintPass{pass}, nil
		}
		names = append(names, pass.name)
	}

	return nil, fmt.Errorf("--lint-pass %q is no lint pass: the passes are %s", name, strings.Join(names, ", "))
}

// unlintedFileException is a Go file no lint pass builds, with the reason no
// pass can.
type unlintedFileException struct {
	path   string
	reason string
}

// unlintedFileExceptions are the files no pass builds by design.
// TestUnlintedFileExceptionsAreFilesNoPassBuilds holds each to a file of the
// tree that no pass builds, so an exception cannot outlive its reason.
func unlintedFileExceptions() []unlintedFileException {
	const noSupportedBuild = "built only for a platform the module does not build on (the cross build covers " +
		"linux, darwin and freebsd), where the type check of the module fails before it reads this file"

	return []unlintedFileException{
		{path: "internal/localpath/opendir_other.go", reason: "!unix: " + noSupportedBuild},
		{path: "internal/synctree/birthtime_other.go", reason: "not linux, darwin, freebsd or netbsd: " + noSupportedBuild},
	}
}

// filesNoLintPassBuilds lists, relative to repoRoot and slash-separated, the
// Go files of the module that no lint pass builds, the declared exceptions
// left out. It walks the tree the way `./...` does: a directory named
// testdata or vendor, or whose name starts with `.` or `_`, holds no package
// of the module, and neither does a directory with its own go.mod. This
// differs from the repository rules' skip set (skipRepositoryRuleDir) on
// purpose: it follows the go command's own pattern rules, since a file the
// go command never builds is no file a linter could read.
func filesNoLintPassBuilds(repoRoot string) ([]string, error) {
	unread := []string{}
	err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %s: %w", path, err)
		}
		name := entry.Name()
		if entry.IsDir() {
			if path == repoRoot {
				return nil
			}
			if skipsGoPackageDir(name) || holdsOwnModule(path) {
				return filepath.SkipDir
			}

			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			return nil
		}
		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return fmt.Errorf("relativize %s: %w", path, relErr)
		}
		rel = filepath.ToSlash(rel)
		built, buildErr := builtByALintPass(filepath.Dir(path), name)
		if buildErr != nil {
			return buildErr
		}
		if !built && !isUnlintedFileException(rel) {
			unread = append(unread, rel)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list the Go files no lint pass builds: %w", err)
	}

	return unread, nil
}

// skipsGoPackageDir reports whether the go command's `./...` pattern leaves
// out a directory of this name.
func skipsGoPackageDir(name string) bool {
	return name == goTestdataDirName || name == verifyVendorDir || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// holdsOwnModule reports whether dir is the root of a nested module.
func holdsOwnModule(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "go.mod"))

	return err == nil
}

// builtByALintPass reports whether some lint pass builds the file.
func builtByALintPass(dir string, name string) (bool, error) {
	for _, pass := range lintPasses() {
		built, err := pass.buildsFile(dir, name)
		if err != nil || built {
			return built, err
		}
	}

	return false, nil
}

func isUnlintedFileException(rel string) bool {
	for _, exception := range unlintedFileExceptions() {
		if exception.path == rel {
			return true
		}
	}

	return false
}

// checkEveryFileIsLinted fails when a Go file of the tree is built by no
// lint pass and is no declared exception.
func checkEveryFileIsLinted(repoRoot string) error {
	unread, err := filesNoLintPassBuilds(repoRoot)
	if err != nil {
		return err
	}
	if len(unread) == 0 {
		return nil
	}

	return fmt.Errorf("lint: no lint pass builds %s: add a pass to lintPasses that builds it, "+
		"or declare it in unlintedFileExceptions with the reason no pass can", strings.Join(unread, ", "))
}

// lintStepRun is the lint step over passes: the check that every file is
// linted, then each pass over the packages packagesToLint names. Every pass
// runs whatever an earlier one returned, so a failure on one platform does
// not hide another's, and the step fails with all of them.
func lintStepRun(passes []lintPass, packagesToLint lintScope) func(
	context.Context, commandRunner, string, []string, io.Writer, io.Writer,
) error {
	return func(ctx context.Context, runner commandRunner, repoRoot string, env []string, stdout, stderr io.Writer) error {
		if err := writeStatus(stdout, "==> golangci-lint\n"); err != nil {
			return fmt.Errorf("write status: %w", err)
		}
		if err := assertLintVersionMatchesCI(ctx, runner, repoRoot, env); err != nil {
			return err
		}
		coverageErr := checkEveryFileIsLinted(repoRoot)
		packages, lintNone, err := packagesToLint(ctx, runner, repoRoot, env, stdout)
		if err != nil || lintNone {
			return errors.Join(coverageErr, err)
		}

		base := append(lintCommandArgs(runtime.GOMAXPROCS(0)), packages...)
		passErrs := []error{coverageErr}
		for _, pass := range passes {
			if len(passes) > 1 {
				if err := writeStatus(stdout, fmt.Sprintf("    pass %s: GOOS=%s, tags %s\n",
					pass.name, pass.goos, strings.Join(pass.buildTags(), ","))); err != nil {
					return fmt.Errorf("write status: %w", err)
				}
			}
			args := pass.commandArgs(base)
			if err := runner.Run(ctx, repoRoot, pass.env(env), stdout, stderr, lintBinaryName, args...); err != nil {
				passErrs = append(passErrs, fmt.Errorf("lint pass %s: %w", pass.name, err))
			}
		}

		return errors.Join(passErrs...)
	}
}

// lintScope says which packages the lint step lints: the arguments that
// follow the shared ones (none for the whole module), or that it lints
// nothing.
type lintScope func(ctx context.Context, runner commandRunner, repoRoot string, env []string, stdout io.Writer) (
	packages []string, lintNone bool, err error)

// lintWholeModule lints every package.
func lintWholeModule(context.Context, commandRunner, string, []string, io.Writer) ([]string, bool, error) {
	return nil, false, nil
}
