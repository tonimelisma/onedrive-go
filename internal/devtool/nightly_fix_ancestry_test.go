package devtool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	ancestryTestFix  = "1111111111111111111111111111111111111111"
	ancestryTestHead = "2222222222222222222222222222222222222222"
)

// ancestryRunner answers git merge-base --is-ancestor from a scripted list of
// exit codes and records every git command, so a test sees whether the check
// fetched the commits it could not find.
type ancestryRunner struct {
	mergeBaseExits []int
	fetchFails     bool
	commands       []string
}

func (r *ancestryRunner) Run(_ context.Context, _ string, _ []string, _, _ io.Writer, name string, args ...string) error {
	r.commands = append(r.commands, name+" "+strings.Join(args, " "))
	switch {
	case len(args) > 0 && args[0] == nightlyTestMergeBase:
		code := r.mergeBaseExits[0]
		if len(r.mergeBaseExits) > 1 {
			r.mergeBaseExits = r.mergeBaseExits[1:]
		}
		if code == 0 {
			return nil
		}
		return fmt.Errorf("git merge-base: %w", nightlyExitError{code: code})
	case len(args) > 0 && args[0] == "fetch":
		if r.fetchFails {
			return fmt.Errorf("git fetch: %w", nightlyExitError{code: 128})
		}
		return nil
	}
	return fmt.Errorf("unexpected command: %s %s", name, strings.Join(args, " "))
}

func (r *ancestryRunner) Output(context.Context, string, []string, string, ...string) ([]byte, error) {
	return nil, errors.New("unexpected Output call")
}

func (r *ancestryRunner) CombinedOutput(context.Context, string, []string, string, ...string) ([]byte, error) {
	return nil, errors.New("unexpected CombinedOutput call")
}

// Validates: R-6.10.23
func TestFixIsInRun_FetchesACommitTheCloneLacksAndAsksAgain(t *testing.T) {
	t.Parallel()

	runner := &ancestryRunner{mergeBaseExits: []int{128, 0}}

	inRun, err := fixIsInRun(t.Context(), runner, t.TempDir(), ancestryTestFix, ancestryTestHead)

	require.NoError(t, err)
	assert.True(t, inRun)
	require.Len(t, runner.commands, 3)
	assert.Equal(t, "git fetch --quiet --no-tags origin "+ancestryTestFix+" "+ancestryTestHead, runner.commands[1])
}

// Validates: R-6.10.23
func TestFixIsInRun_ACommitNoFetchFindsIsNotProvablyInTheRun(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		fetchFails bool
	}{
		{name: "fetch refused", fetchFails: true},
		{name: "still missing after fetch", fetchFails: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			runner := &ancestryRunner{mergeBaseExits: []int{128}, fetchFails: tc.fetchFails}

			inRun, err := fixIsInRun(t.Context(), runner, t.TempDir(), ancestryTestFix, ancestryTestHead)

			require.NoError(t, err, "a commit the clone cannot find must not fail the nightly check")
			assert.False(t, inRun)
		})
	}
}

// Validates: R-6.10.23
func TestFixIsInRun_AnsweredAncestryNeedsNoFetch(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		exit  int
		inRun bool
	}{
		{name: "ancestor", exit: 0, inRun: true},
		{name: "not an ancestor", exit: 1, inRun: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			runner := &ancestryRunner{mergeBaseExits: []int{tc.exit}}

			inRun, err := fixIsInRun(t.Context(), runner, t.TempDir(), ancestryTestFix, ancestryTestHead)

			require.NoError(t, err)
			assert.Equal(t, tc.inRun, inRun)
			assert.Len(t, runner.commands, 1)
		})
	}
}
