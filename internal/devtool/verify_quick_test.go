package devtool

import (
	"bytes"
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		RepoRoot: repoRoot,
		Profile:  VerifyQuick,
		Stdout:   stdout,
		Stderr:   &bytes.Buffer{},
	})
	require.NoError(t, err)

	// No lint (its own CI job), no race, no coverage profile, no compile
	// guard and no live suite: the repository rules, the builds, the tagged
	// vets and the short unit tier.
	assert.Equal(t, []string{
		"gofumpt -w .",
		"goimports -local github.com/tonimelisma/onedrive-go",
		"go build ./...",
		"go build ./... GOOS=linux",
		"go build ./... GOOS=darwin",
		"go build ./... GOOS=freebsd",
		"go vet -tags=e2e e2e_full",
		"go vet -tags=integration",
		"go test -short",
	}, recordedCommandSequence(runner))

	unit := runner.runCommands[len(runner.runCommands)-1]
	assert.Equal(t, []string{"test", "-short", "-timeout=" + shortUnitTestTimeout, "./..."}, unit.args)
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
		RepoRoot: repoRoot,
		Profile:  VerifyQuick,
		Shard:    "2/3",
		Stdout:   stdout,
		Stderr:   &bytes.Buffer{},
	})
	require.NoError(t, err)

	unit := runner.runCommands[len(runner.runCommands)-1]
	assert.Equal(t, []string{"test", "-short", "-timeout=" + shortUnitTestTimeout, "-run=^(.{3})*.{1}$", "./..."}, unit.args)
	assert.Contains(t, stdout.String(), "==> go test -short (shard 2/3)\n")
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

// Validates: R-6.10.22
func TestRunVerifyPlatformRunsHostBuildAndShortTier(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot: t.TempDir(),
		Profile:  VerifyPlatform,
		Shard:    "1/2",
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	})
	require.NoError(t, err)

	// The secondary-OS leg: no format tools to install, no cross-compiles,
	// no repository rules (they are platform-independent), just the host
	// build and the short unit tier.
	assert.Equal(t, []string{"go build ./...", "go test -short"}, recordedCommandSequence(runner))
	assert.Equal(t,
		[]string{"test", "-short", "-timeout=" + shortUnitTestTimeout, "-run=^(.{2})*$", "./..."},
		runner.runCommands[1].args,
	)
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
	assert.Contains(t, err.Error(), "--shard applies to the quick and platform profiles only")
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
