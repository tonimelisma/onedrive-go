package devtool

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.10.26
//
// The format step used to rewrite the tree and return nil, so a nightly that
// formats before it lints could never be red on format. With --check-format
// it lists what each formatter would change and fails when anything would.
func TestRunVerifyCheckFormatFailsOnUnformattedFiles(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{
		runStdoutFunc: func(args []string) string {
			switch {
			case len(args) == 2 && args[0] == "-l":
				return "internal/a.go\n"
			case len(args) > 0 && args[0] == "-local":
				return "internal/b.go\n"
			default:
				return ""
			}
		},
	}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:    t.TempDir(),
		Profile:     VerifyQuick,
		CheckFormat: true,
		Stdout:      &bytes.Buffer{},
		Stderr:      &bytes.Buffer{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "internal/a.go")
	assert.Contains(t, err.Error(), "internal/b.go")

	require.GreaterOrEqual(t, len(runner.runCommands), 2)
	assert.Equal(t, "gofumpt", runner.runCommands[0].name)
	assert.Equal(t, []string{"-l", "."}, runner.runCommands[0].args)
	assert.Equal(t, "goimports", runner.runCommands[1].name)
	assert.Equal(t, []string{"-local", repoModulePath, "-l", "."}, runner.runCommands[1].args)
	for _, command := range runner.runCommands {
		assert.NotContains(t, command.args, "-w", "a check never rewrites the tree")
	}
}

// Validates: R-6.10.26
func TestCheckFormatPassesOnAFormattedTree(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	require.NoError(t, checkFormat(context.Background(), runner, t.TempDir(), nil, &bytes.Buffer{}, &bytes.Buffer{}))
	require.Len(t, runner.runCommands, len(formatterCommands()))
}

// Validates: R-6.10.26
func TestRunVerifyRejectsCheckFormatWithoutAFormatStep(t *testing.T) {
	t.Parallel()

	for _, profile := range []VerifyProfile{VerifyLint, VerifyE2E, VerifyStress} {
		runner := &fakeRunner{}
		err := RunVerify(context.Background(), runner, &VerifyOptions{
			RepoRoot:    t.TempDir(),
			Profile:     profile,
			CheckFormat: true,
			Stdout:      &bytes.Buffer{},
			Stderr:      &bytes.Buffer{},
		})
		require.ErrorContains(t, err, "--check-format", "profile %s", profile)
		assert.Empty(t, runner.runCommands, "profile %s", profile)
	}
}

// Validates: R-6.10.26
//
// A lint failure used to end the public profile before the unit tests, so the
// nightly reported one failure and hid the state of every test.
func TestRunVerifyPublicRunsUnitTestsAfterLintFails(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRequirementsBackedReadme(t, repoRoot)
	writeCoverageRequirement(t, repoRoot, testCoverageRequirement)
	coverFile := filepath.Join(repoRoot, "cover.out")
	summaryPath := filepath.Join(t.TempDir(), "verify-summary.json")
	runner := &fakeRunner{
		outputs:     map[string][]byte{"go tool cover -func=" + coverFile: []byte("total:\t(statements)\t90.0%\n")},
		runErrByKey: map[string]error{lintCommandKey(): assert.AnError},
	}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:        repoRoot,
		Profile:         VerifyPublic,
		CoverageFile:    coverFile,
		SummaryJSONPath: summaryPath,
		Stdout:          &bytes.Buffer{},
		Stderr:          &bytes.Buffer{},
	})
	require.ErrorContains(t, err, "lint")

	var summary VerifySummary
	readVerifySummaryFile(t, summaryPath, &summary)
	assert.Equal(t, verifySummaryStatusFail, requireVerifySummaryStep(t, summary, "lint").Status)
	assertVerifySummaryHasStep(t, summary, "build")
	assertVerifySummaryHasStep(t, summary, "unit tests")
	assertVerifySummaryHasStep(t, summary, "coverage")
	assertVerifySummaryHasStep(t, summary, fullE2ECompileStepName)
}

// Validates: R-6.10.26
//
// Without a complete coverage profile the gate would report a second,
// misleading failure, so it is recorded as not run; the compile guard is
// independent and still runs.
func TestRunVerifyPublicSkipsOnlyTheCoverageGateAfterUnitTestsFail(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRequirementsBackedReadme(t, repoRoot)
	writeCoverageRequirement(t, repoRoot, testCoverageRequirement)
	coverFile := filepath.Join(repoRoot, "cover.out")
	unitTimeout := 20 * time.Minute
	unitTestKey := strings.Join([]string{
		"go", "test", "-race", "-timeout=" + unitTimeout.String(), "-coverpkg=./...", "-coverprofile=" + coverFile, "./...",
	}, " ")
	summaryPath := filepath.Join(t.TempDir(), "verify-summary.json")
	runner := &fakeRunner{runErrByKey: map[string]error{unitTestKey: assert.AnError}}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:        repoRoot,
		Profile:         VerifyPublic,
		CoverageFile:    coverFile,
		UnitTimeout:     unitTimeout,
		SummaryJSONPath: summaryPath,
		Stdout:          &bytes.Buffer{},
		Stderr:          &bytes.Buffer{},
	})
	require.ErrorContains(t, err, "unit tests")
	assert.Empty(t, runner.outputCommands, "the coverage gate does not read a partial profile")

	var summary VerifySummary
	readVerifySummaryFile(t, summaryPath, &summary)
	assert.Equal(t, verifySummaryStatusNotRun, requireVerifySummaryStep(t, summary, "coverage").Status)
	assertVerifySummaryHasStep(t, summary, fullE2ECompileStepName)
}

// Validates: R-6.10.26
//
// The full tier's go test over ./... passed a run in which no package ran a
// test, and the coverage gate then measured whatever profile it was given.
func TestRunVerifyPublicFailsAUnitTierThatRanNoTest(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRequirementsBackedReadme(t, repoRoot)
	writeCoverageRequirement(t, repoRoot, testCoverageRequirement)
	coverFile := filepath.Join(repoRoot, "cover.out")
	runner := &fakeRunner{
		outputs: map[string][]byte{"go tool cover -func=" + coverFile: []byte("total:\t(statements)\t90.0%\n")},
		runStdoutFunc: func(args []string) string {
			if slices.Contains(args, "-coverprofile="+coverFile) {
				return "?   \t" + repoModulePath + "\t[no test files]\n"
			}

			return passingGoTestOutput(args)
		},
	}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:     repoRoot,
		Profile:      VerifyPublic,
		CoverageFile: coverFile,
		Stdout:       &bytes.Buffer{},
		Stderr:       &bytes.Buffer{},
	})
	require.ErrorContains(t, err, "unit tests matched no tests")
}

// Validates: R-6.10.8, R-6.10.26
func TestRunVerifyStressRunsEveryStepAfterOneFails(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{
		runErrByKey: map[string]error{
			"go test -race -count=50 -shuffle=on -timeout=" + stressMultisyncTimeout + " ./internal/multisync": assert.AnError,
		},
	}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot: t.TempDir(),
		Profile:  VerifyStress,
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	})
	require.ErrorContains(t, err, "stress tests")
	require.Len(t, runner.runCommands, 3, "the sync x50 and x3 steps still run")
}
