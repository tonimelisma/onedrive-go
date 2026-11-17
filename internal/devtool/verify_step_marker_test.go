package devtool

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
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

// verifyStepNameForm is the form of every step name: lower-case words and
// digits joined by hyphens.
var verifyStepNameForm = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// markerLines are the lines of output that start with the failed-step
// marker's prefix, in order.
func markerLines(output string) []string {
	lines := []string{}
	for line := range strings.Lines(output) {
		if strings.HasPrefix(line, verifyStepFailedPrefix) {
			lines = append(lines, strings.TrimSuffix(line, "\n"))
		}
	}

	return lines
}

// Validates: R-6.10.23, R-6.10.26
//
// A failed step names itself in one line on standard error, after what the
// step printed there: the prefix, the profile and the step's name, and nothing
// else. The steps that passed before it print no marker. The prefix is spelled
// out here, not taken from its constant, because the nightly reader reads it
// in logs an older devtool wrote.
func TestRunVerifyPrintsOneMarkerLineForAFailedStepAndNoneForAPassingOne(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRequirementsBackedReadme(t, repoRoot)
	buildKey := goCommandKey([]string{"build", "./..."})
	runner := &fakeRunner{
		runErrByKey: map[string]error{buildKey: assert.AnError},
		runStderrFunc: func(args []string) string {
			if goCommandKey(args) == buildKey {
				return "the build's own error output\n"
			}

			return ""
		},
	}
	stderr := &bytes.Buffer{}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot: repoRoot,
		Profile:  VerifyQuick,
		Stdout:   &bytes.Buffer{},
		Stderr:   stderr,
	})
	require.ErrorContains(t, err, "build")

	assert.Equal(t, "the build's own error output\nverify step failed: quick build\n", stderr.String(),
		"one marker, after the step's output; the format and rule steps that passed print none")
}

// Validates: R-6.10.23, R-6.10.26
//
// The public profile runs every step whatever an earlier one returned, so
// each failed step prints its own marker, in the order the steps ran, a failed
// go test step too. A step recorded as not run prints none.
func TestRunVerifyPublicPrintsAMarkerForEveryFailedStepInOrder(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRequirementsBackedReadme(t, repoRoot)
	writeCoverageRequirement(t, repoRoot, testCoverageRequirement)
	coverFile := filepath.Join(repoRoot, "cover.out")
	unitTimeout := 20 * time.Minute
	unitTestKey := goCommandKey([]string{
		"test", "-race", "-timeout=" + unitTimeout.String(), "-coverpkg=./...", "-coverprofile=" + coverFile, "./...",
	})
	runner := &fakeRunner{runErrByKey: map[string]error{lintCommandKey(): assert.AnError, unitTestKey: assert.AnError}}
	stderr := &bytes.Buffer{}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:     repoRoot,
		Profile:      VerifyPublic,
		CoverageFile: coverFile,
		UnitTimeout:  unitTimeout,
		Stdout:       &bytes.Buffer{},
		Stderr:       stderr,
	})
	require.Error(t, err)

	assert.Equal(t, []string{
		verifyStepFailedPrefix + "public lint",
		verifyStepFailedPrefix + "public unit-tests",
	}, markerLines(stderr.String()))
}

// Validates: R-6.10.23
//
// Step names are a closed set: each is a constant of type verifyStepName,
// lower-case words joined by hyphens, and verifyStepNames lists exactly the
// declared ones. Every step and bucket every profile runs is named from the
// set, every name of the set is a step or bucket some profile runs, and no
// profile names two of its steps alike, so a failed step's marker and its
// nightly key always say which step it was, and the reader accepts no name
// that no step prints.
func TestVerifyProfilesNameEveryStepOnceFromTheClosedSet(t *testing.T) {
	t.Parallel()

	declared := declaredVerifyStepNames(t)
	require.NotEmpty(t, declared)
	assert.ElementsMatch(t, declared, verifyStepNames(), "verifyStepNames lists every declared step name and nothing else")
	for _, name := range declared {
		assert.Regexp(t, verifyStepNameForm, string(name))
	}
	assert.Len(t, uniqueStepNames(verifyStepNames()), len(verifyStepNames()), "two constants share a name")

	run := map[verifyStepName]bool{}
	for _, profile := range strings.Split(VerifyProfileUsage, "|") {
		names := profileStepNames(t, VerifyProfile(profile))
		require.NotEmpty(t, names, "profile %s runs no step", profile)
		assert.Len(t, uniqueStepNames(names), len(names), "profile %s names two steps alike: %v", profile, names)
		for _, name := range names {
			assert.Contains(t, verifyStepNames(), name, "profile %s runs a step whose name is outside the set", profile)
			run[name] = true
		}
	}
	assert.ElementsMatch(t, verifyStepNames(), slices.Collect(maps.Keys(run)),
		"every name of the set is a step or bucket some profile runs")
}

// Validates: R-6.10.23
//
// Each name of the closed set says whether its step runs go test, which
// tells the nightly reader whether a failed test can name the step's
// failure. The two groups name every declared constant, so a new step gets
// a deliberate answer here as well as in the exhaustive switch. A name
// outside the set runs no go test, so its marker would never be dropped.
func TestVerifyStepNamesSayWhetherTheyRunGoTest(t *testing.T) {
	t.Parallel()

	goTestSteps := []verifyStepName{
		unitTestsStepName, shortUnitTestsStepName, authPreflightStepName, fastFixturePreflightStepName,
		fastE2EStepName, fullFixturePreflightStepName, fullE2EParallelMiscBucketName, fullE2ESerialSyncBucketName,
		fullE2ESerialWatchSharedBucketName, integrationStepName, stressMultisyncRaceStepName,
		stressSyncConcurrencyRaceStepName, stressSyncRaceStepName, raceStepName, seedsStepName, failedTestRerunStepName,
	}
	otherSteps := []verifyStepName{
		formatStepName, lintStepName, workflowLintStepName, outputBoundariesStepName, testConventionsStepName,
		readmeStatusStepName, specGovernsStepName, layeringStepName, retiredBacklogIDsStepName,
		mutexOwnershipDocsStepName, requirementTraceabilityStepName, buildStepName, hostBuildStepName,
		taggedVetStepName, coverageStepName, fullE2ECompileStepName, fullE2EDiscoveryStepName,
	}
	assert.ElementsMatch(t, declaredVerifyStepNames(t), append(slices.Clone(goTestSteps), otherSteps...),
		"every declared step name is in exactly one group")
	for _, name := range goTestSteps {
		assert.True(t, name.runsGoTest(), "%s runs go test", name)
	}
	for _, name := range otherSteps {
		assert.False(t, name.runsGoTest(), "%s runs no go test", name)
	}
	assert.False(t, verifyStepName("not-a-step").runsGoTest(), "a name outside the set is keyed whenever it appears")
}

func uniqueStepNames(names []verifyStepName) map[verifyStepName]bool {
	unique := map[verifyStepName]bool{}
	for _, name := range names {
		unique[name] = true
	}

	return unique
}

// declaredVerifyStepNames reads the value of every constant of type
// verifyStepName that the package's non-test files declare.
func declaredVerifyStepNames(t *testing.T) []verifyStepName {
	t.Helper()

	dir := filepath.Join(repoRootFromDevtoolPackage(t), "internal", "devtool")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	fset := token.NewFileSet()
	names := []verifyStepName{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, filepath.Join(dir, entry.Name()), nil, parser.SkipObjectResolution)
		require.NoError(t, parseErr)
		ast.Inspect(file, func(node ast.Node) bool {
			spec, ok := node.(*ast.ValueSpec)
			if !ok {
				return true
			}
			if typeName, isIdent := spec.Type.(*ast.Ident); !isIdent || typeName.Name != "verifyStepName" {
				return true
			}
			for _, value := range spec.Values {
				literal, isLiteral := value.(*ast.BasicLit)
				require.True(t, isLiteral, "%s: a step name is a string literal", entry.Name())
				name, unquoteErr := strconv.Unquote(literal.Value)
				require.NoError(t, unquoteErr)
				names = append(names, verifyStepName(name))
			}

			return true
		})
	}

	return names
}

// profileStepNames runs one profile against a fake runner on which every
// command passes and returns the names of the steps and buckets its summary
// records, in order.
func profileStepNames(t *testing.T, profile VerifyProfile) []verifyStepName {
	t.Helper()

	repoRoot := newFakeE2ERepo(t)
	writeRequirementsBackedReadme(t, repoRoot)
	writeCoverageRequirement(t, repoRoot, testCoverageRequirement)
	coverFile := filepath.Join(repoRoot, "cover.out")
	failedLog := filepath.Join(repoRoot, "ci.log")
	const failedTest = "TestStepNamesFixture"
	require.NoError(t, os.WriteFile(failedLog, []byte(rawFailLines(failedTest)+
		"FAIL\t"+repoModulePath+"/internal/sync\t1.0s\n"), 0o600))
	runner := &fakeRunner{outputs: map[string][]byte{
		"go tool cover -func=" + coverFile: []byte("total:\t(statements)\t90.0%\n"),
	}}
	if profile == VerifyFailedTest {
		runner.runStdoutFunc = goTestReportsPass(failedTest)
	}
	summaryPath := filepath.Join(t.TempDir(), "verify-summary.json")

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:        repoRoot,
		Profile:         profile,
		CoverageFile:    coverFile,
		E2ELogDir:       filepath.Join(t.TempDir(), "e2e-logs"),
		SummaryJSONPath: summaryPath,
		FailedTestLog:   failedLog,
		Stdout:          &bytes.Buffer{},
		Stderr:          &bytes.Buffer{},
	})
	require.NoError(t, err, "profile %s", profile)

	var summary VerifySummary
	readVerifySummaryFile(t, summaryPath, &summary)
	names := []verifyStepName{}
	for _, step := range summary.Steps {
		names = append(names, verifyStepName(step.Name))
	}
	for i := range summary.E2EFullBuckets {
		names = append(names, verifyStepName(summary.E2EFullBuckets[i].Name))
	}

	return names
}
