package devtool

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// lintScopeFullFiles are the files whose change makes the pr profile lint the
// whole tree: they change what every package compiles against or how the
// linter reads it.
func lintScopeFullFiles() []string {
	return []string{"go.mod", "go.sum", ".golangci.yml"}
}

// lintChangedPackages is the pr profile's lint scope: only the packages a
// change against origin/main touches, and the packages that import them
// directly, so the local pre-PR gate does not lint the whole tree for a
// change to a few packages. CI's verify lint job and the full profiles
// (verify public, which nightly CI runs) still lint the
// whole tree. A change to go.mod, go.sum or .golangci.yml lints the whole
// tree here too, and a change with no Go package in it lints nothing. Every
// lint pass lints the same packages.
func lintChangedPackages(ctx context.Context, runner commandRunner, repoRoot string, env []string, stdout io.Writer) (
	[]string, bool, error,
) {
	packages, whole, err := changedLintPackages(ctx, runner, repoRoot, env)
	if err != nil {
		return nil, false, fmt.Errorf("lint: find the packages changed against %s: %w", originMainRef, err)
	}
	switch {
	case whole:
		if err := writeStatus(stdout, "    the change touches the build or the lint config: linting every package\n"); err != nil {
			return nil, false, fmt.Errorf("write status: %w", err)
		}
		return nil, false, nil
	case len(packages) == 0:
		if err := writeStatus(stdout, "    no Go package changed against "+originMainRef+": nothing to lint\n"); err != nil {
			return nil, false, fmt.Errorf("write status: %w", err)
		}
		return nil, true, nil
	default:
		if err := writeStatus(stdout, fmt.Sprintf("    %d changed or directly dependent packages\n", len(packages))); err != nil {
			return nil, false, fmt.Errorf("write status: %w", err)
		}
		return packages, false, nil
	}
}

// changedLintPackages returns, as ./dir arguments in name order, the packages
// with a Go file changed against the merge base with origin/main (committed,
// staged, unstaged or untracked) and the packages that import one of them
// directly, tests included; whole is true when a file of lintScopeFullFiles
// changed.
func changedLintPackages(ctx context.Context, runner commandRunner, repoRoot string, env []string) ([]string, bool, error) {
	base, err := runner.Output(ctx, repoRoot, env, "git", "merge-base", originMainRef, "HEAD")
	if err != nil {
		return nil, false, fmt.Errorf("merge base: %w", err)
	}
	tracked, err := runner.Output(ctx, repoRoot, env, "git", "diff", "--name-only", strings.TrimSpace(string(base)))
	if err != nil {
		return nil, false, fmt.Errorf("changed files: %w", err)
	}
	untracked, err := runner.Output(ctx, repoRoot, env, "git", "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, false, fmt.Errorf("untracked files: %w", err)
	}

	changedDirs := map[string]bool{}
	for _, file := range strings.Fields(string(tracked) + "\n" + string(untracked)) {
		if slices.Contains(lintScopeFullFiles(), file) {
			return nil, true, nil
		}
		if !strings.HasSuffix(file, ".go") {
			continue
		}
		dir := path.Dir(file)
		if info, statErr := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(dir))); statErr == nil && info.IsDir() {
			changedDirs[dir] = true
		}
	}
	if len(changedDirs) == 0 {
		return nil, false, nil
	}

	listed, err := runner.Output(ctx, repoRoot, env, "go", "list", "-e",
		"-f", "{{.Dir}}\t{{.ImportPath}}\t{{join .Imports \" \"}} {{join .TestImports \" \"}} {{join .XTestImports \" \"}}",
		"./...")
	if err != nil {
		return nil, false, fmt.Errorf("list packages: %w", err)
	}

	return lintPackagesFromList(repoRoot, string(listed), changedDirs), false, nil
}

// lintPackagesFromList returns the changed directories and the packages of
// listed (go list lines of directory, import path and imports) that import a
// changed package directly, as ./dir arguments in name order.
func lintPackagesFromList(repoRoot string, listed string, changedDirs map[string]bool) []string {
	type listedPackage struct {
		dir     string
		imports []string
	}
	var packages []listedPackage
	changedImports := map[string]bool{}
	for _, line := range strings.Split(listed, "\n") {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			continue
		}
		rel, err := filepath.Rel(repoRoot, fields[0])
		if err != nil {
			continue
		}
		dir := filepath.ToSlash(rel)
		packages = append(packages, listedPackage{dir: dir, imports: strings.Fields(fields[2])})
		if changedDirs[dir] {
			changedImports[fields[1]] = true
		}
	}

	selected := map[string]bool{}
	for dir := range changedDirs {
		selected[dir] = true
	}
	for _, pkg := range packages {
		for _, imported := range pkg.imports {
			if changedImports[imported] {
				selected[pkg.dir] = true
				break
			}
		}
	}

	args := make([]string, 0, len(selected))
	for dir := range selected {
		if dir == "." {
			args = append(args, ".")
			continue
		}
		args = append(args, "./"+dir)
	}
	slices.Sort(args)

	return args
}
