package devtool

import (
	"bytes"
	"context"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testShortUnitTimeout is an explicit short-tier deadline that is neither
// tier's default on any host, so a test that passes it sees the option reach
// go test rather than a default that happens to match.
const (
	testShortUnitTimeout    = 12 * time.Minute
	testShortUnitTimeoutArg = "-timeout=12m0s"
)

func recordedCommandSequence(runner *fakeRunner) []string {
	sequence := make([]string, 0, len(runner.runCommands))
	for _, command := range runner.runCommands {
		goos := ""
		for _, entry := range command.env {
			if strings.HasPrefix(entry, "GOOS=") {
				goos = " " + entry
			}
		}
		args := command.args
		if len(args) > 2 {
			args = args[:2]
		}
		sequence = append(sequence, command.name+" "+strings.Join(args, " ")+goos)
	}

	return sequence
}

// Validates: R-6.10.22
func TestRunVerifyQuickRunsExpectedSteps(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRequirementsBackedReadme(t, repoRoot)
	runner := &fakeRunner{}
	stdout := &bytes.Buffer{}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:         repoRoot,
		Profile:          VerifyQuick,
		ShortUnitTimeout: testShortUnitTimeout,
		Stdout:           stdout,
		Stderr:           &bytes.Buffer{},
	})
	require.NoError(t, err)

	// No lint (its own CI job), no race, no coverage profile, no compile
	// guard and no live suite: the repository rules, the builds, the tagged
	// vets and the short unit tier. --short-unit-timeout reaches go test as
	// given, whatever the host's core count.
	assert.Equal(t, quickCommandSequence(), recordedCommandSequence(runner))

	unit := runner.runCommands[len(runner.runCommands)-1]
	assert.Equal(t, []string{"test", "-short", testShortUnitTimeoutArg, "./..."}, unit.args)
	assert.Empty(t, runner.outputCommands, "quick reports no coverage")
	assert.Contains(t, stdout.String(), "==> go test -short\n")
	assert.NotContains(t, stdout.String(), "==> coverage")
	assert.NotContains(t, stdout.String(), "==> golangci-lint")
}

// Validates: R-6.10.22
func TestRunVerifyQuickShardSelectsOneSliceOfTheShortTier(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRequirementsBackedReadme(t, repoRoot)
	runner := &fakeRunner{}
	stdout := &bytes.Buffer{}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:         repoRoot,
		Profile:          VerifyQuick,
		Shard:            "2/3",
		ShortUnitTimeout: testShortUnitTimeout,
		Stdout:           stdout,
		Stderr:           &bytes.Buffer{},
	})
	require.NoError(t, err)

	unit := runner.runCommands[len(runner.runCommands)-1]
	assert.Equal(t, []string{"test", "-short", testShortUnitTimeoutArg, "-run=^(.{3})*.{1}$", "./..."}, unit.args)
	assert.Contains(t, stdout.String(), "==> go test -short (shard 2/3)\n")
}

// Validates: R-6.10.22, R-6.10.26
//
// A shard whose pattern selects no test in any package passed: go test prints
// `[no tests to run]` for every package and exits 0, so a shard count or a
// pattern that came apart from the tree went unnoticed.
func TestRunVerifyQuickFailsAShardThatRanNoTest(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRequirementsBackedReadme(t, repoRoot)
	runner := &fakeRunner{
		runStdoutFunc: func(args []string) string {
			if slices.Contains(args, "-short") {
				return "ok  \t" + repoModulePath + "/internal/sync\t0.012s [no tests to run]\n" +
					"?   \t" + repoModulePath + "/internal/synctest\t[no test files]\n"
			}

			return passingGoTestOutput(args)
		},
	}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot: repoRoot,
		Profile:  VerifyQuick,
		Shard:    "2/3",
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	})
	require.ErrorContains(t, err, "short unit tests matched no tests")
}

// Validates: R-6.10.22
func TestRunVerifyLintRunsOnlyLint(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot: t.TempDir(),
		Profile:  VerifyLint,
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"golangci-lint run --allow-parallel-runners"}, recordedCommandSequence(runner))
}

// quickCommandSequence is what the quick profile runs, as
// recordedCommandSequence reports it.
func quickCommandSequence() []string {
	return []string{
		"gofumpt -w .",
		"goimports -local github.com/tonimelisma/onedrive-go",
		"go build ./...",
		"go build ./... GOOS=linux",
		"go build ./... GOOS=darwin",
		"go build ./... GOOS=freebsd",
		"go vet -tags=e2e e2e_full",
		"go vet -tags=integration",
		"go test -short",
	}
}

// Validates: R-6.10.22
//
// The pr profile is the local gate before a pull request: what the required
// PR check runs on Linux, in one command, on a host too small for the full
// race tier. Lint goes first because it fails fastest.
func TestRunVerifyPRRunsLintThenQuick(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRequirementsBackedReadme(t, repoRoot)
	runner := &fakeRunner{}
	stdout := &bytes.Buffer{}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:         repoRoot,
		Profile:          VerifyPR,
		ShortUnitTimeout: testShortUnitTimeout,
		Stdout:           stdout,
		Stderr:           &bytes.Buffer{},
	})
	require.NoError(t, err)

	expected := append([]string{"golangci-lint run --allow-parallel-runners"}, quickCommandSequence()...)
	assert.Equal(t, expected, recordedCommandSequence(runner))

	unit := runner.runCommands[len(runner.runCommands)-1]
	assert.Equal(t, []string{"test", "-short", testShortUnitTimeoutArg, "./..."}, unit.args)
	assert.Empty(t, runner.outputCommands, "pr runs no race tier and reports no coverage")
	assert.NotContains(t, stdout.String(), "==> coverage")
}

// Validates: R-6.10.22
func TestRunVerifyPlatformRunsHostBuildAndShortTier(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:         t.TempDir(),
		Profile:          VerifyPlatform,
		Shard:            "1/2",
		ShortUnitTimeout: testShortUnitTimeout,
		Stdout:           &bytes.Buffer{},
		Stderr:           &bytes.Buffer{},
	})
	require.NoError(t, err)

	// The secondary-OS leg: no format tools to install, no cross-compiles,
	// no repository rules (they are platform-independent), just the host
	// build and the short unit tier.
	assert.Equal(t, []string{"go build ./...", "go test -short"}, recordedCommandSequence(runner))
	assert.Equal(t,
		[]string{"test", "-short", testShortUnitTimeoutArg, "-run=^(.{2})*$", "./..."},
		runner.runCommands[1].args,
	)
}

// The short tier's 10-minute deadline fits the CI runners, whose core count is
// the reference. At the reference and above the deadline stays, so no CI job's
// deadline moves past its job timeout. Below it the deadline is 60 core-minutes
// divided by the cores, because the same tests take several times as long on a
// small host that other work shares; a host that reports no cores counts as
// one.
//
// Validates: R-6.10.22
func TestShortUnitTestTimeoutForCoresGivesSmallHostsACoreBudget(t *testing.T) {
	t.Parallel()

	for _, cores := range []int{3, 4, 8} {
		assert.Equal(t, 10*time.Minute, shortUnitTestTimeoutForCores(cores), "cores %d", cores)
	}

	assert.Equal(t, 30*time.Minute, shortUnitTestTimeoutForCores(2))
	assert.Equal(t, 60*time.Minute, shortUnitTestTimeoutForCores(1))
	assert.Equal(t, 60*time.Minute, shortUnitTestTimeoutForCores(0))
}

// Validates: R-6.10.22
func TestResolveShortUnitTestTimeoutPrefersTheExplicitValue(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 15*time.Minute, resolveShortUnitTestTimeout(15*time.Minute, 2))
	assert.Equal(t, 30*time.Minute, resolveShortUnitTestTimeout(0, 2))
	assert.Equal(t, 10*time.Minute, resolveShortUnitTestTimeout(0, 8))
}

// Validates: R-6.10.22
//
// With no --short-unit-timeout, every profile that runs the short tier gives
// go test the deadline of the cores this process may use.
func TestRunVerifyShortTierDeadlineComesFromTheCoresByDefault(t *testing.T) {
	t.Parallel()

	want := "-timeout=" + shortUnitTestTimeoutForCores(runtime.GOMAXPROCS(0)).String()

	for _, profile := range []VerifyProfile{VerifyQuick, VerifyPR, VerifyPlatform} {
		repoRoot := t.TempDir()
		writeRequirementsBackedReadme(t, repoRoot)
		runner := &fakeRunner{}

		err := RunVerify(context.Background(), runner, &VerifyOptions{
			RepoRoot: repoRoot,
			Profile:  profile,
			Stdout:   &bytes.Buffer{},
			Stderr:   &bytes.Buffer{},
		})
		require.NoError(t, err, "profile %s", profile)

		unit := runner.runCommands[len(runner.runCommands)-1]
		assert.Equal(t, []string{"test", "-short", want, "./..."}, unit.args, "profile %s", profile)
	}
}

// Validates: R-6.10.22
func TestVerifyShardRejectsBadSpecsAndOtherProfiles(t *testing.T) {
	t.Parallel()

	for _, spec := range []string{"1", "0/2", "3/2", "a/2", "1/b", "1/0"} {
		_, err := parseVerifyShard(spec)
		require.Error(t, err, spec)
	}

	err := RunVerify(context.Background(), &fakeRunner{}, &VerifyOptions{
		RepoRoot: t.TempDir(),
		Profile:  VerifyPublic,
		Shard:    "1/2",
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--shard applies to the pr, quick and platform profiles only")
}

// Validates: R-6.10.22
//
// Running every shard of one count must run every test exactly once, whatever
// the test is called, so the shard patterns must partition all names.
func TestVerifyShardPatternsPartitionEveryName(t *testing.T) {
	t.Parallel()

	names := []string{
		"TestA",
		"TestRunOnce_RandomizedBidirectionalEditsConverge",
		"TestReverseDispatch_RunOnceScenariosPass",
		"FuzzDriveItemUnmarshal",
		"ExampleX",
	}
	for length := range 80 {
		names = append(names, "Test"+strings.Repeat("x", length))
	}

	for count := 1; count <= 4; count++ {
		patterns := make([]*regexp.Regexp, 0, count)
		for index := 1; index <= count; index++ {
			shard, err := parseVerifyShard(strconv.Itoa(index) + "/" + strconv.Itoa(count))
			require.NoError(t, err)
			patterns = append(patterns, regexp.MustCompile(shard.runPattern()))
		}
		for _, name := range names {
			matches := 0
			for _, pattern := range patterns {
				if pattern.MatchString(name) {
					matches++
				}
			}
			assert.Equal(t, 1, matches, "count %d name %q", count, name)
		}
	}
}

func TestVerifyShardSingleSliceRunsTheWholeTier(t *testing.T) {
	t.Parallel()

	shard, err := parseVerifyShard("1/1")
	require.NoError(t, err)
	assert.False(t, shard.enabled())
}
