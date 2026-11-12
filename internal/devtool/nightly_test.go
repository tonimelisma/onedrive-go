package devtool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	nightlyTestRunID   = 4242
	nightlyTestRunHead = "aaaaaaaaaaaaaaaa"
	nightlyTestOwner   = "fix/this-increment"
	nightlyTestGit     = "git"
	nightlyTestOther   = "fix/other-agent"
	nightlyTestKey     = "TestE2E_RapidFileChurn"
)

type nightlyExitError struct{ code int }

func (e nightlyExitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func (e nightlyExitError) ExitCode() int { return e.code }

// claimRemote is a commandRunner that behaves like the shared remote for
// claim refs: pushes are compare-and-swap, and a hook can move a ref between
// an agent's read and its push to model a concurrent agent.
type claimRemote struct {
	t          *testing.T
	refs       map[string]string
	commits    map[string]string
	nextCommit int
	runs       string
	jobs       string
	jobLogs    map[string]string
	prBody     string
	prBranch   string
	ancestors  map[string]bool
	beforePush func(remote *claimRemote)
	pushes     []string
	// jobLogErrs are the failures the job-log fetch of a job id answers
	// with, in order, before it returns the log; jobLogCalls counts the
	// fetches.
	jobLogErrs  map[string][]error
	jobLogCalls map[string]int
	// sleeps records the backoffs the check waited out.
	sleeps recordedSleeps
}

func newClaimRemote(t *testing.T, conclusion string) *claimRemote {
	t.Helper()

	runs := fmt.Sprintf(
		`[{"databaseId":%d,"conclusion":%q,"headSha":%q,"createdAt":"2026-11-09T10:00:00Z","url":"https://example.invalid/run"}]`,
		nightlyTestRunID, conclusion, nightlyTestRunHead,
	)
	return &claimRemote{
		t:           t,
		refs:        map[string]string{},
		commits:     map[string]string{},
		runs:        runs,
		jobs:        `{"jobs":[]}`,
		jobLogs:     map[string]string{},
		ancestors:   map[string]bool{},
		jobLogErrs:  map[string][]error{},
		jobLogCalls: map[string]int{},
	}
}

func (r *claimRemote) seed(key string, claim nightlyClaim) {
	r.t.Helper()

	claim.Key = key
	record, err := json.Marshal(claim)
	require.NoError(r.t, err)
	r.refs[key] = r.commit(string(record))
}

func (r *claimRemote) commit(message string) string {
	r.nextCommit++
	sha := fmt.Sprintf("c%03d", r.nextCommit)
	r.commits[sha] = message
	return sha
}

func (r *claimRemote) claim(key string) (nightlyClaim, bool) {
	r.t.Helper()

	sha, ok := r.refs[key]
	if !ok {
		return nightlyClaim{}, false
	}
	var claim nightlyClaim
	require.NoError(r.t, json.Unmarshal([]byte(r.commits[sha]), &claim))
	return claim, true
}

func (r *claimRemote) Run(_ context.Context, _ string, _ []string, _, _ io.Writer, name string, args ...string) error {
	if name == nightlyTestGit && len(args) == 4 && args[0] == "merge-base" {
		if r.ancestors[args[2]+".."+args[3]] {
			return nil
		}
		return fmt.Errorf("git merge-base: %w", nightlyExitError{code: 1})
	}
	return nil
}

func (r *claimRemote) Output(_ context.Context, _ string, _ []string, name string, args ...string) ([]byte, error) {
	joined := name + " " + strings.Join(args, " ")
	switch {
	case strings.HasPrefix(joined, "gh run list"):
		return []byte(r.runs), nil
	case strings.HasPrefix(joined, "gh run view"):
		return []byte(r.jobs), nil
	case strings.HasPrefix(joined, "gh api repos/{owner}/{repo}/actions/jobs/"):
		id := strings.TrimSuffix(strings.TrimPrefix(args[1], "repos/{owner}/{repo}/actions/jobs/"), "/logs")
		r.jobLogCalls[id]++
		if failures := r.jobLogErrs[id]; len(failures) > 0 {
			r.jobLogErrs[id] = failures[1:]
			return nil, failures[0]
		}
		return []byte(r.jobLogs[id]), nil
	case strings.HasPrefix(joined, "gh pr view"):
		body, err := json.Marshal(map[string]string{"body": r.prBody, "headRefName": r.prBranch})
		if err != nil {
			return nil, fmt.Errorf("encode pr view: %w", err)
		}
		return body, nil
	case strings.HasPrefix(joined, "git for-each-ref"):
		keys := make([]string, 0, len(r.refs))
		for key := range r.refs {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, key := range keys {
			sha := r.refs[key]
			fmt.Fprintf(&b, "%s\t%s%s\t%s\n", sha, nightlyClaimRefPrefix, key, r.commits[sha])
		}
		return []byte(b.String()), nil
	case strings.HasPrefix(joined, "git commit-tree"):
		return []byte(r.commit(args[len(args)-1]) + "\n"), nil
	case strings.HasPrefix(joined, "git rev-parse --abbrev-ref HEAD"):
		return []byte(nightlyTestOwner + "\n"), nil
	}
	return nil, fmt.Errorf("unexpected command: %s", joined)
}

func (r *claimRemote) CombinedOutput(_ context.Context, _ string, _ []string, name string, args ...string) ([]byte, error) {
	if name != nightlyTestGit || args[0] != "push" {
		return nil, fmt.Errorf("unexpected command: %s %v", name, args)
	}
	if r.beforePush != nil {
		hook := r.beforePush
		r.beforePush = nil
		hook(r)
	}
	lease := strings.TrimPrefix(args[2], "--force-with-lease="+nightlyClaimRefPrefix)
	key, expect, _ := strings.Cut(lease, ":")
	source, _, _ := strings.Cut(args[4], ":")
	if r.refs[key] != expect {
		return []byte("!\t" + args[4] + "\t[rejected] (stale info)\n"), errors.New("exit status 1")
	}
	r.pushes = append(r.pushes, args[4])
	if source == "" {
		delete(r.refs, key)
	} else {
		r.refs[key] = source
	}
	return nil, nil
}

var _ commandRunner = (*claimRemote)(nil)

func nightlyTestNow() time.Time {
	return time.Date(2026, 11, 9, 20, 0, 0, 0, time.UTC)
}

func failingNightlyRemote(t *testing.T) *claimRemote {
	t.Helper()

	remote := newClaimRemote(t, checkConclusionFailure)
	remote.jobs = `{"jobs":[
		{"databaseId":1,"name":"e2e","conclusion":"failure"},
		{"databaseId":2,"name":"verify","conclusion":"failure"},
		{"databaseId":3,"name":"stress","conclusion":"success"}]}`
	remote.jobLogs["1"] = "2026-11-09T10:20:00Z === RUN   TestE2E_RapidFileChurn\n" +
		"2026-11-09T10:21:00Z --- FAIL: TestE2E_RapidFileChurn (49.50s)\n" +
		"2026-11-09T10:21:00Z     --- FAIL: TestE2E_RapidFileChurn/sub (1.00s)\n"
	return remote
}

func runNightlyCheck(t *testing.T, remote *claimRemote) (string, error) {
	t.Helper()

	var stdout bytes.Buffer
	err := RunNightly(t.Context(), remote, NightlyOptions{
		RepoRoot: "/repo",
		Action:   NightlyActionCheck,
		Branch:   nightlyTestOwner,
		Stdout:   &stdout,
		Now:      nightlyTestNow,
		Sleep:    remote.sleeps.sleep,
	})
	return stdout.String(), err
}

// Validates: R-6.10.23
func TestNightlyFailureKeysNameTestsOrJobs(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.jobs = `{"jobs":[
		{"databaseId":1,"name":"e2e","conclusion":"failure"},
		{"databaseId":4,"name":"verify-full (ubuntu-latest)","conclusion":"failure"},
		{"databaseId":5,"name":"verify-full (macos-latest)","conclusion":"failure"},
		{"databaseId":2,"name":"verify","conclusion":"failure"}]}`
	remote.jobLogs["4"] = `{"Action":"fail","Package":"example/internal/sync","Test":"TestPlanner/case"}` + "\n" +
		`{"Action":"fail","Package":"example/internal/sync"}` + "\n"
	remote.jobLogs["5"] = "the build failed before any test ran\n"

	keys, err := loadNightlyFailureKeys(t.Context(), newGHClient(remote, nil), "/repo", &githubRun{DatabaseID: nightlyTestRunID})
	require.NoError(t, err)
	assert.Equal(t, []string{"TestE2E_RapidFileChurn", "TestPlanner", "job-verify-full--macos-latest"}, keys)
}

// Validates: R-6.10.23
//
// GitHub answers a share of requests with a 5xx, and one such answer to a
// job-log fetch used to end the DoD start stage. The fetch is repeated after
// a backoff and the check goes on.
func TestNightlyCheck_RetriesAJobLog5xxThenSucceeds(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.jobLogErrs["1"] = []error{githubBadGateway()}

	out, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	assert.Equal(t, 2, remote.jobLogCalls["1"])
	assert.Equal(t, githubBackoffs()[:1], remote.sleeps.waits)
	claim, ok := remote.claim(nightlyTestKey)
	require.True(t, ok)
	assert.Equal(t, nightlyTestOwner, claim.Owner)
	assert.Contains(t, out, nightlyTestKey+": claimed by this increment")
}

// Validates: R-6.10.23
//
// When GitHub keeps failing, the check gives up after the retry bound and
// says so: which call, and what GitHub answered. It claims nothing, since it
// does not know what failed.
func TestNightlyCheck_SaysGitHubFailedAfterTheRetryBound(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.jobLogErrs["1"] = slices.Repeat([]error{githubBadGateway()}, githubAttempts+1)

	_, err := runNightlyCheck(t, remote)
	require.Error(t, err)

	assert.Contains(t, err.Error(), "nightly job log e2e")
	assert.Contains(t, err.Error(), fmt.Sprintf("GitHub answered HTTP 502 on all %d attempts", githubAttempts))
	assert.Equal(t, githubAttempts, remote.jobLogCalls["1"])
	assert.Equal(t, githubBackoffs(), remote.sleeps.waits)
	assert.Empty(t, remote.pushes)
	assert.Empty(t, remote.refs)
}

// Validates: R-6.10.23
func TestNightlyCheckClaimsUnownedFailure(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	out, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	claim, ok := remote.claim(nightlyTestKey)
	require.True(t, ok)
	assert.Equal(t, nightlyStateClaimed, claim.State)
	assert.Equal(t, nightlyTestOwner, claim.Owner)
	assert.Equal(t, int64(nightlyTestRunID), claim.RunID)
	assert.Contains(t, out, nightlyTestKey+": claimed by this increment")
}

// Validates: R-6.10.23
func TestNightlyCheckSkipsFailureHeldByAnotherIncrement(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.seed(nightlyTestKey, nightlyClaim{
		State: nightlyStateClaimed,
		Owner: nightlyTestOther,
		At:    nightlyTestNow().Add(-time.Hour).Format(time.RFC3339),
	})

	out, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	claim, _ := remote.claim(nightlyTestKey)
	assert.Equal(t, nightlyTestOther, claim.Owner)
	assert.Empty(t, remote.pushes)
	assert.Contains(t, out, "being fixed elsewhere")
}

// Validates: R-6.10.23
func TestNightlyCheckLosesRaceToConcurrentClaim(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.beforePush = func(remote *claimRemote) {
		remote.seed(nightlyTestKey, nightlyClaim{
			State: nightlyStateClaimed,
			Owner: nightlyTestOther,
			At:    nightlyTestNow().Format(time.RFC3339),
		})
	}

	out, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	claim, _ := remote.claim(nightlyTestKey)
	assert.Equal(t, nightlyTestOther, claim.Owner, "the agent that pushed first keeps the claim")
	assert.Empty(t, remote.pushes)
	assert.Contains(t, out, "being fixed elsewhere")
}

// Validates: R-6.10.23
func TestNightlyCheckTakesOverExpiredClaim(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.seed(nightlyTestKey, nightlyClaim{
		State: nightlyStateClaimed,
		Owner: nightlyTestOther,
		At:    nightlyTestNow().Add(-NightlyClaimLease).Format(time.RFC3339),
	})

	out, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	claim, _ := remote.claim(nightlyTestKey)
	assert.Equal(t, nightlyTestOwner, claim.Owner)
	assert.Contains(t, out, "taken over from "+nightlyTestOther)
}

// Validates: R-6.10.23
func TestNightlyCheckRenewsOwnClaim(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.seed(nightlyTestKey, nightlyClaim{
		State: nightlyStateClaimed,
		Owner: nightlyTestOwner,
		At:    nightlyTestNow().Add(-23 * time.Hour).Format(time.RFC3339),
	})

	_, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	claim, _ := remote.claim(nightlyTestKey)
	assert.Equal(t, nightlyTestNow().Format(time.RFC3339), claim.At)
}

// Validates: R-6.10.23
func TestNightlyCheckWaitsForMergedFixTheRunDidNotContain(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.seed(nightlyTestKey, nightlyClaim{State: nightlyStateFixed, Owner: nightlyTestOther, FixCommit: "fix1"})

	out, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	claim, _ := remote.claim(nightlyTestKey)
	assert.Equal(t, nightlyStateFixed, claim.State)
	assert.Contains(t, out, "awaiting the next nightly")
}

// Validates: R-6.10.23
func TestNightlyCheckReclaimsFailureWhoseFixDidNotHold(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	remote.seed(nightlyTestKey, nightlyClaim{State: nightlyStateFixed, Owner: nightlyTestOther, FixCommit: "fix1"})
	remote.ancestors["fix1.."+nightlyTestRunHead] = true

	out, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	claim, _ := remote.claim(nightlyTestKey)
	assert.Equal(t, nightlyStateClaimed, claim.State)
	assert.Equal(t, nightlyTestOwner, claim.Owner)
	assert.Contains(t, out, "did not hold")
}

// Validates: R-6.10.23
func TestNightlyCheckFailsWhenNoIncrementCanOwnTheFailure(t *testing.T) {
	t.Parallel()

	remote := failingNightlyRemote(t)
	var stdout bytes.Buffer
	err := checkNightly(t.Context(), remote, newGHClient(remote, nil), "/repo", "", nightlyTestNow, &stdout)
	require.Error(t, err)
	assert.Contains(t, err.Error(), nightlyTestKey)
	assert.Contains(t, stdout.String(), "UNCLAIMED")
	assert.Empty(t, remote.refs)
}

// Validates: R-6.10.23
func TestNightlyCheckPrunesConfirmedFixesAndExpiredStaleClaims(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, checkConclusionSuccess)
	remote.seed("TestConfirmed", nightlyClaim{State: nightlyStateFixed, Owner: nightlyTestOther, FixCommit: "fix1"})
	remote.ancestors["fix1.."+nightlyTestRunHead] = true
	remote.seed("TestNotYetRun", nightlyClaim{State: nightlyStateFixed, Owner: nightlyTestOther, FixCommit: "fix2"})
	remote.seed("TestAbandoned", nightlyClaim{
		State: nightlyStateClaimed,
		Owner: nightlyTestOther,
		At:    nightlyTestNow().Add(-2 * NightlyClaimLease).Format(time.RFC3339),
	})
	remote.seed("TestInProgress", nightlyClaim{
		State: nightlyStateClaimed,
		Owner: nightlyTestOther,
		At:    nightlyTestNow().Add(-time.Hour).Format(time.RFC3339),
	})

	_, err := runNightlyCheck(t, remote)
	require.NoError(t, err)

	remaining := sortedRefKeys(remote.refs)
	assert.Equal(t, []string{"TestInProgress", "TestNotYetRun"}, remaining)
}

func sortedRefKeys(refs map[string]string) []string {
	keys := make([]string, 0, len(refs))
	for key := range refs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Validates: R-6.10.23
func TestNightlyPreMergeGateRequiresHeldClaimsInPRBody(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, checkConclusionSuccess)
	remote.prBranch = nightlyTestOwner
	remote.seed(nightlyTestKey, nightlyClaim{State: nightlyStateClaimed, Owner: nightlyTestOwner})
	remote.seed("TestSomeoneElses", nightlyClaim{State: nightlyStateClaimed, Owner: nightlyTestOther})

	remote.prBody = "## Summary\nUnrelated change."
	err := requireNightlyClaimsInPR(t.Context(), remote, newGHClient(remote, nil), "/repo", 7)
	require.Error(t, err)
	assert.Contains(t, err.Error(), nightlyTestKey)
	assert.NotContains(t, err.Error(), "TestSomeoneElses")

	remote.prBody = "## Nightly failures\n- " + nightlyTestKey + ": reruns sync until it converges."
	require.NoError(t, requireNightlyClaimsInPR(t.Context(), remote, newGHClient(remote, nil), "/repo", 7))
}

// Validates: R-6.10.23
func TestNightlyPostMergeMarksHeldClaimsFixed(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, checkConclusionSuccess)
	remote.seed(nightlyTestKey, nightlyClaim{State: nightlyStateClaimed, Owner: nightlyTestOwner})
	remote.seed("TestSomeoneElses", nightlyClaim{State: nightlyStateClaimed, Owner: nightlyTestOther})

	require.NoError(t, markNightlyClaimsFixed(t.Context(), remote, "/repo", nightlyTestOwner, "merge1", nightlyTestNow()))

	mine, _ := remote.claim(nightlyTestKey)
	assert.Equal(t, nightlyStateFixed, mine.State)
	assert.Equal(t, "merge1", mine.FixCommit)
	other, _ := remote.claim("TestSomeoneElses")
	assert.Equal(t, nightlyStateClaimed, other.State)
}

// Validates: R-6.10.23
func TestNightlyReleaseDeletesOnlyTheHoldersClaim(t *testing.T) {
	t.Parallel()

	remote := newClaimRemote(t, checkConclusionSuccess)
	remote.seed(nightlyTestKey, nightlyClaim{State: nightlyStateClaimed, Owner: nightlyTestOther})

	release := NightlyOptions{RepoRoot: "/repo", Action: NightlyActionRelease, Branch: nightlyTestOwner, Key: nightlyTestKey}
	require.Error(t, RunNightly(t.Context(), remote, release))
	assert.Contains(t, remote.refs, nightlyTestKey)

	release.Branch = nightlyTestOther
	require.NoError(t, RunNightly(t.Context(), remote, release))
	assert.NotContains(t, remote.refs, nightlyTestKey)
}
