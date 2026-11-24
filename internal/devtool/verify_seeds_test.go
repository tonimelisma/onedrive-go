package devtool

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
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

// seedsTestNight is a night of the fixtures: day 317 after the epoch.
func seedsTestNight() time.Time {
	return time.Date(2026, time.November, 14, 10, 17, 0, 0, time.UTC)
}

// runSeedsProfile runs the seeds profile against runner on the fixture night
// and returns its stdout.
func runSeedsProfile(t *testing.T, runner *fakeRunner, seedStart uint64) (string, error) {
	t.Helper()

	var stdout bytes.Buffer
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:  t.TempDir(),
		Profile:   VerifySeeds,
		SeedStart: seedStart,
		Now:       seedsTestNight,
		Stdout:    &stdout,
		Stderr:    &bytes.Buffer{},
	})

	return stdout.String(), err
}

// Validates: R-6.10.25
//
// The seeds profile runs the five families' tests over the sync tree, without
// the race detector, every family from the night's start seed with its
// count, and the single-seed variables emptied so a seed in the caller's
// environment cannot replace the range. The start comes from the UTC date,
// or from --seed-start.
func TestRunVerifySeedsSetsRotatedStart(t *testing.T) {
	t.Parallel()

	const nightStart = "317001"
	cases := map[string]struct {
		seedStart uint64
		start     string
	}{
		"the night's start": {start: nightStart},
		"--seed-start":      {seedStart: 42, start: "42"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			runner := &fakeRunner{}
			out, err := runSeedsProfile(t, runner, tc.seedStart)
			require.NoError(t, err)

			require.Len(t, runner.runCommands, 1)
			command := runner.runCommands[0]
			assert.Equal(t, []string{"test", "-count=1", "-timeout=" + seedsTimeout, "-run", seedsRunPattern(), "./internal/sync/..."}, command.args)
			assert.NotContains(t, command.args, "-race")
			assert.NotContains(t, command.args, "-short")
			for _, entry := range []string{
				"ONEDRIVE_GO_MODEL_SEED_START=" + tc.start,
				"ONEDRIVE_GO_PLANNER_SEED_START=" + tc.start,
				"ONEDRIVE_GO_CHAOS_SEED_START=" + tc.start,
				"ONEDRIVE_GO_WATCH_SEED_START=" + tc.start,
				"ONEDRIVE_GO_MODEL_SEEDS=" + strconv.Itoa(seedsEngineCount),
				"ONEDRIVE_GO_PLANNER_SEEDS=" + strconv.Itoa(seedsPlannerCount),
				"ONEDRIVE_GO_CHAOS_SEEDS=" + strconv.Itoa(seedsChaosCount),
				"ONEDRIVE_GO_WATCH_SEEDS=" + strconv.Itoa(seedsWatchCount),
				"ONEDRIVE_GO_MODEL_SEED=",
				"ONEDRIVE_GO_PLANNER_SEED=",
				"ONEDRIVE_GO_DISPATCH_ORDER=",
			} {
				assertCommandHasEnvVar(t, command, entry)
			}
			starts := "==> model seeds start=" + tc.start + " watch=4 engine and crash=12 planner=500 chaos=2\n"
			require.Contains(t, out, starts)
			assert.Less(t, strings.Index(out, starts), strings.Index(out, "ok  \t"), "the start is printed before go test runs")
		})
	}

	night := seedsTestNight()
	assert.Equal(t, uint64(317001), nightlySeedStart(night))
	assert.Equal(t, nightlySeedStart(night), nightlySeedStart(night.Add(13*time.Hour)), "a rerun later that UTC day runs the same seeds")
	assert.Equal(t, nightlySeedStart(night)+nightlySeedStride, nightlySeedStart(night.Add(14*time.Hour)), "the next UTC day")
	assert.Equal(t, uint64(1), nightlySeedStart(nightlySeedEpoch()))
	assert.Equal(t, uint64(1), nightlySeedStart(nightlySeedEpoch().Add(-time.Hour)), "a clock before the epoch")
}

// Validates: R-6.10.25
//
// The start advances by the stride every day, and the stride is at least
// every family's count, so the ranges of two nights never share a seed.
func TestNightlySeedStrideCoversEveryCount(t *testing.T) {
	t.Parallel()

	for name, count := range seedsCounts() {
		assert.GreaterOrEqual(t, nightlySeedStride, count, "%s=%d overlaps the next night", name, count)
		assert.Positive(t, count, name)
	}
	assert.Len(t, seedsCounts(), 4, "engine and crash, planner, chaos, watch")
}

// Validates: R-6.10.25
//
// The run pattern names the family table's tests, which exist in
// the sync tree, so a rename cannot leave the nightly searching nothing, and
// every variable the profile sets is one the model tests read.
func TestSeedsRunPatternNamesExistingTests(t *testing.T) {
	t.Parallel()

	const familyCount = 5
	sources := syncTestSources(t)
	names := strings.Split(strings.TrimSuffix(strings.TrimPrefix(seedsRunPattern(), "^("), ")$"), "|")
	require.Len(t, names, familyCount, "watch, engine, crash, planner, chaos")
	for _, name := range names {
		assert.Regexp(t, `(?m)^func `+regexp.QuoteMeta(name)+`\(t \*testing\.T\) \{$`, sources,
			"the seeds pattern names %s, which the sync tree does not define", name)
	}
	for _, name := range []string{
		seedsModelSeedEnv, seedsModelCountEnv, seedsModelStartEnv,
		seedsPlannerSeedEnv, seedsPlannerCountEnv, seedsPlannerStartEnv,
		seedsChaosCountEnv, seedsChaosStartEnv, seedsWatchCountEnv, seedsWatchStartEnv, seedsDispatchOrderEnv,
	} {
		assert.Contains(t, sources, `"`+name+`"`, "the sync tree's tests read no variable %s", name)
	}
}

// Validates: R-6.10.25
//
// The replay line has one writer, the sync fixtures' LogSeedReplayOnFailure,
// and one reader, here. The writer's prefix and dispatch-order variable are
// this reader's, each family's test is in the package the table names (the
// writer names the package its caller runs in), and each family of the table
// runs its seeds as seed=<n> subtests that call the writer, inside the
// subtest, with the family's own variable, so a family that stops printing its
// line fails here, not on the night it is needed. No other test hands the writer its own t: a test that
// prints replay lines and is not in the table is a family the reader does not
// know.
func TestSeededFamiliesLogTheirReplayLineWithTheirVariable(t *testing.T) {
	t.Parallel()

	files := parseSyncTestFiles(t)
	consts := syncStringConsts(t, files)
	assert.Equal(t, seedReplayPrefix, consts["SeedReplayPrefix"], "the writer's prefix")
	assert.Equal(t, seedsDispatchOrderEnv, consts["DispatchOrderEnv"], "the dispatch-order variable")
	families := []string{}
	for _, family := range seededFamilies() {
		families = append(families, family.test)
		fn := syncTestFunc(files, family.test)
		require.NotNil(t, fn, "the sync tree defines no %s", family.test)
		assert.NotNil(t, syncTestFunc(parsePackageTestFiles(t, family.pkg), family.test),
			"%s is a test of %s, the package the writer names in its replays", family.test, family.pkg)
		assert.Equal(t, []string{family.seedEnv}, replayWriterVariables(fn.Body, consts),
			"%s calls LogSeedReplayOnFailure once, with %s", family.test, family.seedEnv)
		subtest := seedSubtestBody(fn)
		require.NotNil(t, subtest, "%s names each seed's subtest %s<n>", family.test, seedSubtestPrefix)
		assert.Len(t, replayWriterVariables(subtest, consts), 1,
			"%s calls LogSeedReplayOnFailure inside the seed's subtest, whose name the line's reader needs", family.test)
	}
	slices.Sort(families)
	assert.Equal(t, families, testsHandingTheWriterTheirT(files),
		"the tests that call logSeedReplayOnFailure with their own t are the families of the table")
}

// testsHandingTheWriterTheirT are the top-level functions of files that call
// LogSeedReplayOnFailure with the identifier t, sorted: the tests whose
// failing seeds print a replay line. A test of the writer itself hands it a
// fake and is not one of them.
func testsHandingTheWriterTheirT(files []*ast.File) []string {
	names := []string{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			if !isFunc || fn.Body == nil {
				continue
			}
			calls := false
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if isCall && isReplayWriter(call.Fun) && len(call.Args) > 0 && isIdent(call.Args[0], "t") {
					calls = true
				}

				return true
			})
			if calls {
				names = append(names, fn.Name.Name)
			}
		}
	}
	slices.Sort(names)

	return names
}

// syncTreeSources are the paths of the sync tree's test files and of its
// test-fixture package, which holds the replay writer.
func syncTreeSources(t *testing.T) []string {
	t.Helper()

	root := filepath.Join(repoRootFromDevtoolPackage(t), "internal", "sync")
	fixtures := filepath.Join(root, "internal", "syncfixture")
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == goTestdataDirName {
				return filepath.SkipDir
			}

			return nil
		}
		if strings.HasSuffix(path, "_test.go") || (filepath.Dir(path) == fixtures && strings.HasSuffix(path, ".go")) {
			paths = append(paths, path)
		}

		return nil
	})
	require.NoError(t, err)

	return paths
}

// parseSyncTestFiles parses the sync tree's test files and its test-fixture
// package.
func parseSyncTestFiles(t *testing.T) []*ast.File {
	t.Helper()

	return parseGoFiles(t, syncTreeSources(t))
}

// parsePackageTestFiles parses the test files of one package, pkg from the
// module root.
func parsePackageTestFiles(t *testing.T, pkg string) []*ast.File {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(repoRootFromDevtoolPackage(t), filepath.FromSlash(pkg), "*_test.go"))
	require.NoError(t, err)

	return parseGoFiles(t, paths)
}

// parseGoFiles parses the files at paths.
func parseGoFiles(t *testing.T, paths []string) []*ast.File {
	t.Helper()

	fset := token.NewFileSet()
	files := make([]*ast.File, 0, len(paths))
	for _, path := range paths {
		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		require.NoError(t, parseErr, path)
		files = append(files, file)
	}

	return files
}

// syncStringConsts are the string constants files declare, by name.
func syncStringConsts(t *testing.T, files []*ast.File) map[string]string {
	t.Helper()

	consts := map[string]string{}
	for _, file := range files {
		for _, decl := range file.Decls {
			if gen, isGen := decl.(*ast.GenDecl); isGen && gen.Tok == token.CONST {
				addStringConsts(t, gen, consts)
			}
		}
	}

	return consts
}

// addStringConsts adds the string constants of one const declaration.
func addStringConsts(t *testing.T, gen *ast.GenDecl, consts map[string]string) {
	t.Helper()

	for _, spec := range gen.Specs {
		value, isValue := spec.(*ast.ValueSpec)
		if !isValue || len(value.Values) != len(value.Names) {
			continue
		}
		for i, name := range value.Names {
			if lit, isLit := value.Values[i].(*ast.BasicLit); isLit && lit.Kind == token.STRING {
				text, err := strconv.Unquote(lit.Value)
				require.NoError(t, err, name.Name)
				consts[name.Name] = text
			}
		}
	}
}

// syncTestFunc is the top-level function name the files declare, nil for
// none.
func syncTestFunc(files []*ast.File, name string) *ast.FuncDecl {
	for _, file := range files {
		for _, decl := range file.Decls {
			if fn, isFunc := decl.(*ast.FuncDecl); isFunc && fn.Recv == nil && fn.Name.Name == name {
				return fn
			}
		}
	}

	return nil
}

// replayWriterVariables are the values of the variable argument of every
// LogSeedReplayOnFailure call in body, each a constant of consts, or its
// expression's kind when it is none.
func replayWriterVariables(body *ast.BlockStmt, consts map[string]string) []string {
	variables := []string{}
	ast.Inspect(body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall || !isReplayWriter(call.Fun) || len(call.Args) < 2 {
			return true
		}
		variable := fmt.Sprintf("%T, not a constant", call.Args[1])
		name := ""
		switch arg := call.Args[1].(type) {
		case *ast.Ident:
			name = arg.Name
		case *ast.SelectorExpr:
			name = arg.Sel.Name
		}
		if value, isConst := consts[name]; isConst {
			variable = value
		}
		variables = append(variables, variable)

		return true
	})

	return variables
}

// seedSubtestBody is the body of the subtest fn runs under a name made of
// seedSubtestPrefix and a seed, t.Run("seed="+..., func(...) {...}), nil when
// fn runs none.
func seedSubtestBody(fn *ast.FuncDecl) *ast.BlockStmt {
	var body *ast.BlockStmt
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall || len(call.Args) != 2 {
			return true
		}
		selector, isSelector := call.Fun.(*ast.SelectorExpr)
		name, isConcat := call.Args[0].(*ast.BinaryExpr)
		subtest, isLiteral := call.Args[1].(*ast.FuncLit)
		if !isSelector || selector.Sel.Name != "Run" || !isConcat || name.Op != token.ADD || !isLiteral {
			return true
		}
		if lit, isLit := name.X.(*ast.BasicLit); isLit && lit.Value == strconv.Quote(seedSubtestPrefix) {
			body = subtest.Body
		}

		return true
	})

	return body
}

// isIdent reports whether expr is the identifier name.
// isReplayWriter reports a call of the replay writer: the fixture package's
// LogSeedReplayOnFailure, qualified or not.
func isReplayWriter(fun ast.Expr) bool {
	if sel, ok := fun.(*ast.SelectorExpr); ok {
		return sel.Sel.Name == "LogSeedReplayOnFailure"
	}

	return isIdent(fun, "LogSeedReplayOnFailure")
}

func isIdent(expr ast.Expr, name string) bool {
	ident, ok := expr.(*ast.Ident)

	return ok && ident.Name == name
}

// syncTestSources is the text of the sync tree's test files and its
// test-fixture package.
func syncTestSources(t *testing.T) string {
	t.Helper()

	var sources strings.Builder
	for _, path := range syncTreeSources(t) {
		data, readErr := readFile(path)
		require.NoError(t, readErr)
		sources.Write(data)
	}

	return sources.String()
}

// Validates: R-6.10.25
//
// A failing seed fails the profile, and the step ends by printing every
// failing seed as the command that replays it.
func TestRunVerifySeedsPrintsTheReplayOfEveryFailingSeed(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{
		runErr: assert.AnError,
		runStdoutFunc: func([]string) string {
			return "        test_helpers_test.go:480: " + seedReplayPrefix + testEngineReplay + "\n" +
				"    --- FAIL: TestRunOnce_RandomizedBidirectionalEditsConverge/seed=7001 (9.00s)\n" +
				"--- FAIL: TestRunOnce_RandomizedBidirectionalEditsConverge (9.00s)\n" +
				"FAIL\t" + repoModulePath + "/internal/sync\t9.1s\n"
		},
	}
	out, err := runSeedsProfile(t, runner, 0)
	require.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, out, "==> failing seeds, each with the command that replays it:\n  "+testEngineReplay+"\n")
}

// Validates: R-6.10.25, R-6.10.26
//
// A seeds run whose pattern selects no test verifies nothing, so the step
// fails instead of passing on go test's `[no tests to run]`.
func TestRunVerifySeedsFailsWhenNoPackageRanATest(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{runStdoutFunc: func([]string) string {
		return "ok  \t" + repoModulePath + "/internal/sync\t0.011s [no tests to run]\n"
	}}
	_, err := runSeedsProfile(t, runner, 1)
	require.ErrorContains(t, err, string(seedsStepName)+" matched no tests")
}

// Validates: R-6.10.25
//
// --seed-start belongs to the seeds profile; any other refuses it.
func TestRunVerifyRefusesASeedStartOutsideTheSeedsProfile(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:  t.TempDir(),
		Profile:   VerifyRace,
		SeedStart: 1,
		Stdout:    &bytes.Buffer{},
		Stderr:    &bytes.Buffer{},
	})
	require.ErrorContains(t, err, "--seed-start applies to the seeds profile only")
	assert.Empty(t, runner.runCommands)
}
