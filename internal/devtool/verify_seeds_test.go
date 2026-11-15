package devtool

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
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
// The seeds profile runs the four families' tests over the sync tree, without
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
				"ONEDRIVE_GO_MODEL_SEEDS=" + strconv.Itoa(seedsEngineCount),
				"ONEDRIVE_GO_PLANNER_SEEDS=" + strconv.Itoa(seedsPlannerCount),
				"ONEDRIVE_GO_CHAOS_SEEDS=" + strconv.Itoa(seedsChaosCount),
				"ONEDRIVE_GO_MODEL_SEED=",
				"ONEDRIVE_GO_PLANNER_SEED=",
				"ONEDRIVE_GO_DISPATCH_ORDER=",
			} {
				assertCommandHasEnvVar(t, command, entry)
			}
			starts := "==> model seeds start=" + tc.start + " engine and crash=12 planner=8 chaos=2\n"
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
	assert.Len(t, seedsCounts(), 3, "engine and crash, planner, chaos")
}

// Validates: R-6.10.25
//
// The run pattern names the family table's tests, which exist in
// internal/sync, so a rename cannot leave the nightly searching nothing, and
// every variable the profile sets is one the model tests read.
func TestSeedsRunPatternNamesExistingTests(t *testing.T) {
	t.Parallel()

	const familyCount = 4
	sources := syncTestSources(t)
	names := strings.Split(strings.TrimSuffix(strings.TrimPrefix(seedsRunPattern(), "^("), ")$"), "|")
	require.Len(t, names, familyCount, "engine, crash, planner, chaos")
	for _, name := range names {
		assert.Regexp(t, `(?m)^func `+regexp.QuoteMeta(name)+`\(t \*testing\.T\) \{$`, sources,
			"the seeds pattern names %s, which internal/sync does not define", name)
	}
	for _, name := range []string{
		seedsModelSeedEnv, seedsModelCountEnv, seedsModelStartEnv,
		seedsPlannerSeedEnv, seedsPlannerCountEnv, seedsPlannerStartEnv,
		seedsChaosCountEnv, seedsChaosStartEnv, seedsDispatchOrderEnv,
	} {
		assert.Contains(t, sources, `"`+name+`"`, "internal/sync's tests read no variable %s", name)
	}
}

// Validates: R-6.10.25
//
// The replay line has one writer, internal/sync's logSeedReplayOnFailure, and
// one reader, here. The writer's prefix, package and dispatch-order variable
// are this reader's, and each family of the table runs its seeds as seed=<n>
// subtests that call the writer, inside the subtest, with the family's own
// variable, so a family that stops printing its line fails here, not on the
// night it is needed. No other test hands the writer its own t: a test that
// prints replay lines and is not in the table is a family the reader does not
// know.
func TestSeededFamiliesLogTheirReplayLineWithTheirVariable(t *testing.T) {
	t.Parallel()

	files := parseSyncTestFiles(t)
	consts := syncStringConsts(t, files)
	assert.Equal(t, seedReplayPrefix, consts["seedReplayPrefix"], "the writer's prefix")
	assert.Equal(t, seedFamilyPackage, consts["seedReplayPackage"], "the writer's package")
	assert.Equal(t, seedsDispatchOrderEnv, consts["dispatchOrderEnv"], "the dispatch-order variable")
	families := []string{}
	for _, family := range seededFamilies() {
		families = append(families, family.test)
		fn := syncTestFunc(files, family.test)
		require.NotNil(t, fn, "internal/sync defines no %s", family.test)
		assert.Equal(t, []string{family.seedEnv}, replayWriterVariables(fn.Body, consts),
			"%s calls logSeedReplayOnFailure once, with %s", family.test, family.seedEnv)
		subtest := seedSubtestBody(fn)
		require.NotNil(t, subtest, "%s names each seed's subtest %s<n>", family.test, seedSubtestPrefix)
		assert.Len(t, replayWriterVariables(subtest, consts), 1,
			"%s calls logSeedReplayOnFailure inside the seed's subtest, whose name the line's reader needs", family.test)
	}
	slices.Sort(families)
	assert.Equal(t, families, testsHandingTheWriterTheirT(files),
		"the tests that call logSeedReplayOnFailure with their own t are the families of the table")
}

// testsHandingTheWriterTheirT are the top-level functions of files that call
// logSeedReplayOnFailure with the identifier t, sorted: the tests whose
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
				if isCall && isIdent(call.Fun, "logSeedReplayOnFailure") && len(call.Args) > 0 && isIdent(call.Args[0], "t") {
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

// parseSyncTestFiles parses internal/sync's test files.
func parseSyncTestFiles(t *testing.T) []*ast.File {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(repoRootFromDevtoolPackage(t), "internal", "sync", "*_test.go"))
	require.NoError(t, err)
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
// logSeedReplayOnFailure call in body, each a constant of consts, or its
// expression's kind when it is none.
func replayWriterVariables(body *ast.BlockStmt, consts map[string]string) []string {
	variables := []string{}
	ast.Inspect(body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall || !isIdent(call.Fun, "logSeedReplayOnFailure") || len(call.Args) < 2 {
			return true
		}
		variable := fmt.Sprintf("%T, not a constant", call.Args[1])
		if ident, isIdentArg := call.Args[1].(*ast.Ident); isIdentArg {
			if value, isConst := consts[ident.Name]; isConst {
				variable = value
			}
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
func isIdent(expr ast.Expr, name string) bool {
	ident, ok := expr.(*ast.Ident)

	return ok && ident.Name == name
}

// syncTestSources is the text of internal/sync's test files.
func syncTestSources(t *testing.T) string {
	t.Helper()

	dir := filepath.Join(repoRootFromDevtoolPackage(t), "internal", "sync")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var sources strings.Builder
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		data, readErr := readFile(filepath.Join(dir, entry.Name()))
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
	require.ErrorContains(t, err, seedsStepName+" matched no tests")
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
