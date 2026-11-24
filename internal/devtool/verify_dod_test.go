package devtool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.10.16
func TestDODManifestGenerationPreservesManualClassification(t *testing.T) {
	t.Parallel()

	line := 12
	var response singlePRThreadsResponse
	require.NoError(t, jsonUnmarshalString(`{
	  "data": {
	    "repository": {
	      "pullRequest": {
	          "number": 677,
	          "title": "Fix stale work",
	          "url": "https://github.test/pull/677",
	          "reviewThreads": {
	            "nodes": [{
	              "id": "thread-1",
	              "isResolved": false,
	              "isOutdated": false,
	              "path": "internal/sync/file.go",
	              "line": 12,
	              "comments": {
	                "nodes": [{
	                  "author": {"login": "reviewer"},
	                  "body": "Please fix this",
	                  "url": "https://github.test/comment/1",
	                  "createdAt": "2026-04-30T00:00:00Z"
	                }]
	              }
	            }]
	          }
	      }
	    }
	  }
	}`, &response))

	existing := DODCommentManifest{
		Threads: []DODReviewThread{{
			ThreadID:       "thread-1",
			Classification: DODCommentFixed,
			WhatChanged:    "Changed stale precondition ordering.",
			HowFixed:       "Moved the parent check after convergence.",
			Evidence:       []string{"go test ./internal/sync"},
		}},
	}
	pr := response.Data.Repository.PullRequest
	fetched := reviewThreadsFromPR(pr.Number, pr.Title, pr.URL, pr.ReviewThreads.Nodes)
	manifest := mergeDODManifest(existing, githubRepo{Owner: "tonimelisma", Name: "onedrive-go"}, 20, fetched)

	require.Len(t, manifest.Threads, 1)
	thread := manifest.Threads[0]
	assert.Equal(t, "thread-1", thread.ThreadID)
	assert.Equal(t, 677, thread.SourcePR)
	assert.Equal(t, "internal/sync/file.go", thread.Path)
	assert.Equal(t, &line, thread.Line)
	assert.Equal(t, DODCommentFixed, thread.Classification)
	assert.Equal(t, "Changed stale precondition ordering.", thread.WhatChanged)
	assert.Equal(t, []string{"go test ./internal/sync"}, thread.Evidence)
}

// Validates: R-6.10.16
func TestDODFetchPullRequestReviewThreadsPaginates(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{
		outputFunc: func(_ string, _ []string, name string, args ...string) ([]byte, error) {
			require.Equal(t, githubCLI, name)
			argsText := strings.Join(args, "\n")
			if strings.Contains(argsText, "after=cursor-1") {
				return []byte(`{
				  "data": {
				    "repository": {
				      "pullRequest": {
				        "number": 683,
				        "title": "DoD automation",
				        "url": "https://github.test/pull/683",
				        "reviewThreads": {
				          "pageInfo": {"hasNextPage": false, "endCursor": ""},
				          "nodes": [{"id": "thread-2", "isResolved": true}]
				        }
				      }
				    }
				  }
				}`), nil
			}
			assert.NotContains(t, argsText, "after=")
			return []byte(`{
			  "data": {
			    "repository": {
			      "pullRequest": {
			        "number": 683,
			        "title": "DoD automation",
			        "url": "https://github.test/pull/683",
			        "reviewThreads": {
			          "pageInfo": {"hasNextPage": true, "endCursor": "cursor-1"},
			          "nodes": [{"id": "thread-1", "isResolved": false}]
			        }
			      }
			    }
			  }
			}`), nil
		},
	}

	threads, err := fetchPullRequestReviewThreads(
		context.Background(),
		newGHClient(runner, nil),
		testCleanupRepoRoot,
		githubRepo{Owner: "tonimelisma", Name: "onedrive-go"},
		683,
	)

	require.NoError(t, err)
	require.Len(t, threads, 2)
	assert.Equal(t, "thread-1", threads[0].ThreadID)
	assert.Equal(t, "thread-2", threads[1].ThreadID)
	assert.Len(t, runner.outputCommands, 2)
}

// Validates: R-6.10.16
func TestDODManifestGenerationRefreshesGitHubFields(t *testing.T) {
	t.Parallel()

	existing := DODCommentManifest{
		Threads: []DODReviewThread{{
			ThreadID:       "thread-1",
			Path:           "old/path.go",
			IsResolved:     false,
			Classification: DODCommentFixed,
			WhatChanged:    "Kept the fix.",
			HowFixed:       "Preserved the review-thread notes.",
			Evidence:       []string{"go test ./internal/devtool"},
		}},
	}
	fetched := []DODReviewThread{{
		ThreadID:   "thread-1",
		Path:       "new/path.go",
		IsResolved: true,
	}}

	manifest := mergeDODManifest(existing, githubRepo{Owner: "tonimelisma", Name: "onedrive-go"}, 20, fetched)

	require.Len(t, manifest.Threads, 1)
	thread := manifest.Threads[0]
	assert.Equal(t, "new/path.go", thread.Path)
	assert.True(t, thread.IsResolved)
	assert.Equal(t, DODCommentFixed, thread.Classification)
	assert.Equal(t, "Preserved the review-thread notes.", thread.HowFixed)
}

// Validates: R-6.10.16
func TestDODManifestValidationRequiresManualFields(t *testing.T) {
	t.Parallel()

	manifest := DODCommentManifest{Threads: []DODReviewThread{
		{ThreadID: "missing-classification"},
		{ThreadID: "missing-fixed-fields", Classification: DODCommentFixed},
		{ThreadID: "missing-reason", Classification: DODCommentNonActionable},
		{ThreadID: "resolved-without-fields", IsResolved: true},
	}}

	err := validateDODCommentManifest(manifest)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "missing-classification: missing classification")
	assert.Contains(t, msg, "missing-fixed-fields: missing what_changed")
	assert.Contains(t, msg, "missing-fixed-fields: missing how_fixed")
	assert.Contains(t, msg, "missing-fixed-fields: missing evidence")
	assert.Contains(t, msg, "missing-reason: missing reason")
	assert.NotContains(t, msg, "resolved-without-fields")
}

// Validates: R-6.10.16
func TestDODThreadReplyTemplateIncludesFixEvidence(t *testing.T) {
	t.Parallel()

	body := renderDODThreadReply(&DODReviewThread{
		Classification: DODCommentAlreadyFixed,
		WhatChanged:    "Unknown kind filtering already includes directory-capable paths.",
		HowFixed:       "Verified current main has file-or-directory matching.",
		Evidence:       []string{"TestContentFilter_ShouldObserveUnknownKindIncludesDirectoryCapablePaths", "go run ./cmd/devtool verify public"},
	}, 682, "abc123")

	assert.Contains(t, body, "Addressed in #682 (abc123).")
	assert.Contains(t, body, "What changed: Unknown kind filtering already includes directory-capable paths.")
	assert.Contains(t, body, "How fixed: Verified current main has file-or-directory matching.")
	assert.Contains(t, body, "- TestContentFilter_ShouldObserveUnknownKindIncludesDirectoryCapablePaths")
}

// Validates: R-6.10.16
func TestDODThreadReplyTemplateIncludesNonActionableReason(t *testing.T) {
	t.Parallel()

	body := renderDODThreadReply(&DODReviewThread{
		Classification: DODCommentNonActionable,
		Reason:         "Review tool quota notice only; no code feedback was present.",
	}, 682, "abc123")

	assert.Contains(t, body, "Resolved as non-actionable during #682 (abc123).")
	assert.Contains(t, body, "Reason: Review tool quota notice only")
}

// Validates: R-6.10.16
func TestDODReviewThreadMutationCommands(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{
		"api",
		"graphql",
		"-f",
		"query=mutation($threadId:ID!, $body:String!){addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$threadId, body:$body}){comment{url}}}",
		"-F",
		"threadId=thread-1",
		"-F",
		"body=body text",
	}, buildAddThreadReplyArgs("thread-1", "body text"))

	assert.Equal(t, []string{
		"api",
		"graphql",
		"-f",
		"query=mutation($threadId:ID!){resolveReviewThread(input:{threadId:$threadId}){thread{id isResolved}}}",
		"-F",
		"threadId=thread-1",
	}, buildResolveThreadArgs("thread-1"))
}

// Regression: a manifest left in the root checkout still said a May thread
// was open, so every later pre-merge run replied "Addressed in #<PR>" to that
// long-resolved thread again, crediting whichever PR was merging. Replies now
// go only to threads GitHub still shows open, and each answered thread is
// marked resolved in the manifest.
//
// Validates: R-6.10.16
func TestReplyToManifestThreads_SkipsThreadsAlreadyResolvedOnGitHub(t *testing.T) {
	t.Parallel()

	const stateQuery = "query=query($threadId:ID!){node(id:$threadId){... on PullRequestReviewThread{isResolved}}}"
	liveResolved := map[string]bool{"thread-stale": true, "thread-open": false}
	var mutations []string
	runner := &fakeRunner{
		outputFunc: func(_ string, _ []string, _ string, args ...string) ([]byte, error) {
			threadID := strings.TrimPrefix(args[len(args)-1], "threadId=")
			switch {
			case slices.Contains(args, stateQuery):
				return fmt.Appendf(nil, `{"data":{"node":{"isResolved":%t}}}`, liveResolved[threadID]), nil
			case strings.Contains(strings.Join(args, " "), "addPullRequestReviewThreadReply"):
				mutations = append(mutations, "reply:"+strings.TrimPrefix(args[len(args)-3], "threadId="))
				return []byte(`{"data":{"addPullRequestReviewThreadReply":{"comment":{"url":"u"}}}}`), nil
			default:
				mutations = append(mutations, "resolve:"+threadID)
				return []byte(`{}`), nil
			}
		},
	}
	threads := []DODReviewThread{
		{ThreadID: "thread-stale", Classification: DODCommentFixed, WhatChanged: "w", HowFixed: "h"},
		{ThreadID: "thread-open", Classification: DODCommentFixed, WhatChanged: "w", HowFixed: "h"},
		{ThreadID: "thread-known", IsResolved: true},
	}

	var stdout bytes.Buffer
	summary, changed, err := replyToManifestThreads(t.Context(), newGHClient(runner, nil), "/repo", threads, 790, "abc123", &stdout)
	require.NoError(t, err)

	assert.Equal(t, []string{"reply:thread-open", "resolve:thread-open"}, mutations,
		"only the thread still open on GitHub is answered")
	assert.Equal(t, 1, summary.CommentedAndResolved)
	assert.Equal(t, 2, summary.AlreadyResolved)
	assert.True(t, changed)
	for i := range threads {
		assert.True(t, threads[i].IsResolved, "%s is recorded as resolved", threads[i].ThreadID)
	}

	// A second run with the updated manifest touches nothing.
	mutations = nil
	_, changed, err = replyToManifestThreads(t.Context(), newGHClient(runner, nil), "/repo", threads, 791, "def456", &stdout)
	require.NoError(t, err)
	assert.Empty(t, mutations)
	assert.False(t, changed)
}

// Validates: R-6.10.16
func TestDODPreMergeStatusSummariesIncludeResolutionEvidenceCounts(t *testing.T) {
	t.Parallel()

	status := formatDODThreadResolutionStatus(&DODReviewThread{
		ThreadID:       "thread-1",
		Classification: DODCommentFixed,
		Evidence:       []string{"go test ./internal/devtool", "", "go run ./cmd/devtool verify public"},
	})

	assert.Contains(t, status, "commented and resolved thread-1")
	assert.Contains(t, status, "classification=fixed")
	assert.Contains(t, status, "evidence=2")

	summary := formatDODPreMergeSummary(
		dodReviewThreadApplySummary{CommentedAndResolved: 1, AlreadyResolved: 3},
		dodReviewThreadGateSummary{CurrentPRThreads: 2, RecentMergedThreads: 8},
	)

	assert.Contains(t, summary, "commented_resolved=1")
	assert.Contains(t, summary, "already_resolved=3")
	assert.Contains(t, summary, "current_pr_threads=2")
	assert.Contains(t, summary, "recent_merged_threads=8")
}

// Validates: R-6.10.16
func TestDODPRCIClassification(t *testing.T) {
	t.Parallel()

	snapshot := classifyPRChecks([]prCheckRun{
		{Name: "verify", Status: "COMPLETED", Conclusion: "SUCCESS"},
		{Name: "integration", Status: "COMPLETED", Conclusion: "SUCCESS"},
		{Name: "e2e", Status: "COMPLETED", Conclusion: "SUCCESS"},
		{Name: "stress", Status: "COMPLETED", Conclusion: "SKIPPED"},
	})
	assert.Equal(t, prCheckStatePass, snapshot.state)
	assert.Contains(t, snapshot.reason, "passed")

	snapshot = classifyPRChecks([]prCheckRun{
		{Name: "verify", Status: "COMPLETED", Conclusion: "SUCCESS"},
		{
			Name: "integration", Status: "COMPLETED", Conclusion: "SKIPPED",
			DetailsURL: "https://github.com/o/r/actions/runs/111/job/5",
		},
		{Name: "e2e", Status: "COMPLETED", Conclusion: "SUCCESS"},
	})
	assert.Equal(t, prCheckStateFail, snapshot.state)
	assert.Contains(t, snapshot.reason, "integration concluded skipped")
	assert.Zero(t, snapshot.failedRunID, "a skipped required check is a decision, not a cancellation to rerun")
}

// Validates: R-6.10.16
func TestDODPostMergeCIClassificationRejectsSkippedPushRun(t *testing.T) {
	t.Parallel()

	snapshot := classifyPostMergeRuns([]githubRun{{
		DatabaseID: 222,
		Name:       "CI",
		Event:      "push",
		Status:     checkStatusCompleted,
		Conclusion: checkConclusionSkipped,
		HeadSHA:    "merge-sha",
	}}, "merge-sha")

	assert.Equal(t, prCheckStateFail, snapshot.state)
	assert.Contains(t, snapshot.reason, "every push to main must run the quick lane")
	assert.Zero(t, snapshot.failedRunID, "a skipped push run has no canceled job to rerun")

	snapshot = classifyPostMergeRuns([]githubRun{{
		Name:       "CI",
		Event:      "push",
		Status:     checkStatusCompleted,
		Conclusion: checkConclusionSuccess,
		HeadSHA:    "merge-sha",
	}}, "merge-sha")
	assert.Equal(t, prCheckStatePass, snapshot.state)
	assert.Contains(t, snapshot.reason, "succeeded")
}

// Validates: R-6.10.16
//
// A scheduled or dispatched run can run on the merge commit too, and a red
// nightly there is not the merged pull request's failure: the post-merge
// stage waits for the push run alone.
func TestDODPostMergeCIClassificationIgnoresRunsOfOtherEvents(t *testing.T) {
	t.Parallel()

	nightly := githubRun{
		DatabaseID: 333, Name: "CI", Event: ciEventSchedule, Status: checkStatusCompleted,
		Conclusion: checkConclusionFailure, HeadSHA: "merge-sha",
	}
	push := githubRun{
		DatabaseID: 222, Name: "CI", Event: ciEventPush, Status: checkStatusCompleted,
		Conclusion: checkConclusionSuccess, HeadSHA: "merge-sha",
	}

	snapshot := classifyPostMergeRuns([]githubRun{nightly, push}, "merge-sha")
	assert.Equal(t, prCheckStatePass, snapshot.state)

	snapshot = classifyPostMergeRuns([]githubRun{nightly}, "merge-sha")
	assert.Equal(t, prCheckStatePending, snapshot.state, "only a push run decides; none has been listed yet")
	assert.Zero(t, snapshot.failedRunID)
}

// Validates: R-6.10.16
func TestLoadMainRunsForCommitAsksForPushRunsOnly(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{outputFunc: func(string, []string, string, ...string) ([]byte, error) { return []byte("[]"), nil }}
	_, err := loadMainRunsForCommit(t.Context(), newGHClient(runner, nil), "/repo", "merge-sha")
	require.NoError(t, err)

	require.Len(t, runner.outputCommands, 1)
	args := runner.outputCommands[0].args
	event := slices.Index(args, "--event")
	require.GreaterOrEqual(t, event, 0)
	assert.Equal(t, ciEventPush, args[event+1])
}

// nightlyStageRunner runs a whole DoD stage against two fakes: the nightly
// step's gh commands, the claims' among them, go to a claimRemote, every
// other command to a fakeRunner.
type nightlyStageRunner struct {
	*fakeRunner
	remote *claimRemote
}

// isNightlyStepCommand reports whether a command is one of the nightly
// step's: any gh command.
func isNightlyStepCommand(name string) bool {
	return name == githubCLI
}

func (r *nightlyStageRunner) Run(ctx context.Context, cwd string, env []string, stdout, stderr io.Writer, name string, args ...string) error {
	if isNightlyStepCommand(name) {
		return r.remote.Run(ctx, cwd, env, stdout, stderr, name, args...)
	}
	return r.fakeRunner.Run(ctx, cwd, env, stdout, stderr, name, args...)
}

func (r *nightlyStageRunner) Output(ctx context.Context, cwd string, env []string, name string, args ...string) ([]byte, error) {
	if isNightlyStepCommand(name) {
		return r.remote.Output(ctx, cwd, env, name, args...)
	}
	return r.fakeRunner.Output(ctx, cwd, env, name, args...)
}

func (r *nightlyStageRunner) CombinedOutput(ctx context.Context, cwd string, env []string, name string, args ...string) ([]byte, error) {
	if isNightlyStepCommand(name) {
		return r.remote.CombinedOutput(ctx, cwd, env, name, args...)
	}
	return r.fakeRunner.CombinedOutput(ctx, cwd, env, name, args...)
}

// Validates: R-6.10.16, R-6.10.23
//
// The full race tier cannot finish inside its deadline on a two-core host, so
// a pre-pr stage that demanded it could not be run where the work happens.
// The stage's default is the profile such a host can finish; CI stays the
// full gate. After the gates the stage renews the branch's nightly claim and
// prints the latest verdict.
func TestDODPrePRRunsThePRProfileByDefault(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	remote := failingNightlyRemote(t)
	now := time.Now()
	remote.runs = nightlyRunListAt(t, now, nightlyListedRun{
		id: nightlyTestRunID, status: checkStatusCompleted, conclusion: checkConclusionFailure, age: time.Hour,
	})
	remote.seed(nightlyTestKey, &nightlyClaim{
		State: nightlyStateClaimed, Owner: nightlyTestOwner, At: now.Add(-20 * time.Hour).UTC().Format(time.RFC3339),
	})
	runner := &nightlyStageRunner{fakeRunner: &fakeRunner{}, remote: remote}
	stdout := &bytes.Buffer{}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:  repoRoot,
		DOD:       true,
		DODStage:  DODStagePrePR,
		DODBranch: nightlyTestOwner,
		Stdout:    stdout,
		Stderr:    &bytes.Buffer{},
	})
	require.NoError(t, err)

	// The fake repository has no change against origin/main, so the pr
	// profile's changed-package lint has nothing to run.
	expected := quickCommandSequence()
	expected = append(expected, "git fetch origin", "git merge-base --is-ancestor")
	assert.Equal(t, expected, recordedCommandSequence(runner.fakeRunner))
	assert.False(t, slices.ContainsFunc(runner.outputCommands, func(command recordedCommand) bool {
		return command.name == goCommandName
	}), "the pr profile reports no coverage")
	assert.Contains(t, stdout.String(), "dod pre-pr:")

	require.Len(t, remote.claimWrites, 1, "the stage renews the branch's one claim")
	require.Len(t, remote.claimIssues(), 1)
	assert.True(t, strings.HasPrefix(remote.claimWrites[0],
		fmt.Sprintf("gh api -X POST %s/issues/%d/comments ", githubRepoPath, remote.claimIssues()[0].number)),
		"the renewal is a comment on the claim's issue")
	assert.Contains(t, stdout.String(), "nightly: renewed "+nightlyTestOwner+"'s claim on "+nightlyTestKey+"\n")
	assert.Contains(t, stdout.String(), fmt.Sprintf("nightly: run %d on %s concluded failure", nightlyTestRunID, shortSHA(nightlyTestRunHead)))
	assert.Contains(t, stdout.String(), "  e2e: failed\n")
}

// Validates: R-6.10.16, R-6.10.23
//
// post-merge marks the nightly claims of --branch fixed and restarts that
// branch from main, so a --branch that is not the pull request's head is
// refused before anything is merged.
func TestDODPostMergeRefusesABranchThatIsNotThePRsHead(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{outputs: map[string][]byte{
		"gh api " + githubRepoPath + "/pulls/7": []byte(`{"state":"open","head":{"ref":"fix/the-head","sha":"head-sha"}}`),
	}}
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:  writeWorkflowFixture(t, nightlyBoundWorkflow),
		DOD:       true,
		DODStage:  DODStagePostMerge,
		DODPR:     7,
		DODBranch: "fix/another-branch",
		Stdout:    &bytes.Buffer{},
		Stderr:    &bytes.Buffer{},
	})
	require.Error(t, err)

	assert.Contains(t, err.Error(), "fix/another-branch")
	assert.Contains(t, err.Error(), "fix/the-head")
	assertNoMergeRequest(t, runner)
	assert.Empty(t, runner.runCommands, "no branch is touched")
}

// Validates: R-6.10.16
//
// The branch is reset to main after the merge, so a checkout holding a
// change, tracked or not, is refused before anything is merged.
func TestDODPostMergeRefusesACheckoutWithChanges(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{outputs: map[string][]byte{
		"git rev-parse --abbrev-ref HEAD":              []byte("fix/the-head\n"),
		"gh api " + githubRepoPath + "/pulls/7":        []byte(`{"state":"open","head":{"ref":"fix/the-head","sha":"head-sha"}}`),
		"git status --porcelain --untracked-files=all": []byte("?? notes.txt\n"),
	}}
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot: writeWorkflowFixture(t, nightlyBoundWorkflow),
		DOD:      true,
		DODStage: DODStagePostMerge,
		DODPR:    7,
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	})
	require.Error(t, err)

	assert.Contains(t, err.Error(), "notes.txt")
	assertNoMergeRequest(t, runner)
	assert.Empty(t, runner.runCommands, "no branch is touched")
}

// Validates: R-6.10.16
//
// After the merge the branch restarts from the new origin/main, as a
// session's branch starts its next increment, and is pushed with a lease.
// The fetch prunes every ref: a merge that deleted the remote branch left a
// remote-tracking ref behind, and a lease read from it would refuse the push.
func TestRestartBranchFromMainPointsTheBranchAtMainAndPushesIt(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	require.NoError(t, restartBranchFromMain(context.Background(), runner, testCleanupRepoRoot, "fix/the-head", io.Discard,
		(&recordedSleeps{}).sleep))

	got := make([]string, 0, len(runner.runCommands))
	for _, command := range runner.runCommands {
		got = append(got, command.name+" "+strings.Join(command.args, " "))
	}
	assert.Equal(t, []string{
		"git fetch --prune origin",
		"git checkout -B fix/the-head origin/main",
		"git push --force-with-lease -u origin fix/the-head",
	}, got)
}

// Validates: R-6.10.16
//
// GitHub deletes a merged pull request's branch a moment after the merge, so
// the lease of the first push can name a branch that is gone by the time the
// push lands. The restart fetches again, which prunes it, and pushes again.
func TestRestartBranchFromMainRetriesAPushTheBranchDeletionRaced(t *testing.T) {
	t.Parallel()

	const push = "git push --force-with-lease -u origin fix/the-head"
	runner := &fakeRunner{runErrSequenceByKey: map[string][]error{
		push: {errors.New("remote rejected: cannot lock ref"), nil},
	}}
	sleeps := &recordedSleeps{}
	require.NoError(t, restartBranchFromMain(context.Background(), runner, testCleanupRepoRoot, "fix/the-head", io.Discard,
		sleeps.sleep))

	got := make([]string, 0, len(runner.runCommands))
	for _, command := range runner.runCommands {
		got = append(got, command.name+" "+strings.Join(command.args, " "))
	}
	assert.Equal(t, []string{
		"git fetch --prune origin",
		"git checkout -B fix/the-head origin/main",
		push,
		"git fetch --prune origin",
		push,
	}, got)
	assert.Len(t, sleeps.waits, 1, "the second push waits for GitHub's deletion to land")
}

// Validates: R-6.10.16
func TestRestartBranchFromMainFailsAfterItsPushAttempts(t *testing.T) {
	t.Parallel()

	const push = "git push --force-with-lease -u origin fix/the-head"
	runner := &fakeRunner{runErrByKey: map[string]error{push: errors.New("remote rejected: protected branch")}}
	err := restartBranchFromMain(context.Background(), runner, testCleanupRepoRoot, "fix/the-head", io.Discard,
		(&recordedSleeps{}).sleep)
	require.ErrorContains(t, err, "protected branch")

	pushes := 0
	for _, command := range runner.runCommands {
		if command.name+" "+strings.Join(command.args, " ") == push {
			pushes++
		}
	}
	assert.Equal(t, restartPushAttempts, pushes)
}

// assertNoMergeRequest fails when runner sent a merge request.
func assertNoMergeRequest(t *testing.T, runner *fakeRunner) {
	t.Helper()

	for _, command := range runner.outputCommands {
		assert.NotContains(t, strings.Join(command.args, " "), "/merge", "nothing is merged")
	}
}

// Validates: R-6.10.16
func TestDODPrePRKeepsTheFullTierSelectable(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeCoverageRequirement(t, repoRoot, testCoverageRequirement)
	coverageFile := filepath.Join(repoRoot, "cover.out")
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"go tool cover -func=" + coverageFile: []byte("total:\t(statements)\t76.5%\n"),
		},
	}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:     repoRoot,
		Profile:      VerifyPublic,
		CoverageFile: coverageFile,
		DOD:          true,
		DODStage:     DODStagePrePR,
		Stdout:       &bytes.Buffer{},
		Stderr:       &bytes.Buffer{},
	})
	require.NoError(t, err)

	require.Len(t, runner.runCommands, verifyPublicCommandCount+2, "the full tier, then the rebase check")
	assert.Equal(t, "-race", runner.runCommands[cmdUnitTests].args[1])
}

// mergeRequestKey is the REST merge request mergePR sends for PR 682 at
// head-sha.
const mergeRequestKey = "gh api -X PUT " + githubRepoPath + "/pulls/682/merge -f merge_method=squash -f sha=head-sha"

// Validates: R-6.10.16, R-6.10.40
//
// The merge is a REST squash merge pinned to the head the stage checked, and
// the server's merge state decides the outcome, also when the request failed
// after GitHub carried it out or found the pull request merged already.
func TestDODMergeSquashesOverRESTAndReadsTheServerState(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		mergeErr error
	}{
		{name: "merged by the request"},
		{name: "merged already, the request refused", mergeErr: ghFailure("HTTP 405: Pull Request is not mergeable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			runner := &fakeRunner{outputFunc: func(_ string, _ []string, name string, args ...string) ([]byte, error) {
				switch key := name + " " + strings.Join(args, " "); key {
				case mergeRequestKey:
					return []byte(`{"merged":true}`), tc.mergeErr
				case "gh api " + githubRepoPath + "/pulls/682":
					return []byte(`{"state":"closed","merged":true,"merged_at":"2026-04-30T04:15:18Z","merge_commit_sha":"merge-sha"}`), nil
				default:
					return nil, errors.New("unscripted: " + key)
				}
			}}

			state, err := mergePR(context.Background(), newGHClient(runner, nil), testCleanupRepoRoot, 682, "head-sha")
			require.NoError(t, err)
			assert.True(t, state.Merged)
			assert.Equal(t, "merge-sha", state.MergeCommitSHA)
		})
	}
}

// Validates: R-6.10.16
func TestDODMergeFailsWhenTheServerSaysItIsNotMerged(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{outputFunc: func(_ string, _ []string, name string, args ...string) ([]byte, error) {
		switch key := name + " " + strings.Join(args, " "); key {
		case mergeRequestKey:
			return nil, ghFailure("HTTP 409: Head branch was modified. Review and try the merge again.")
		case "gh api " + githubRepoPath + "/pulls/682":
			// An open pull request carries the merge commit of its test merge:
			// that alone does not make it merged.
			return []byte(`{"state":"open","merged":false,"merge_commit_sha":"test-merge-sha"}`), nil
		default:
			return nil, errors.New("unscripted: " + key)
		}
	}}

	_, err := mergePR(context.Background(), newGHClient(runner, nil), testCleanupRepoRoot, 682, "head-sha")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Head branch was modified")
	assert.Contains(t, err.Error(), "server state is not merged")
}

func jsonUnmarshalString(data string, v any) error {
	decoder := json.NewDecoder(strings.NewReader(data))
	if err := decoder.Decode(v); err != nil {
		return fmt.Errorf("decode json fixture: %w", err)
	}
	return nil
}

// Validates: R-6.10.40
//
// The repository comes from REST, never `gh repo view`, whose GraphQL a
// Claude Code cloud session refuses.
func TestLoadGitHubRepoReadsTheRepositoryOverREST(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{outputs: map[string][]byte{
		"gh api " + githubRepoPath: []byte(`{"full_name":"octo/widgets","name":"widgets"}`),
	}}
	repo, err := loadGitHubRepo(t.Context(), newGHClient(runner, nil), testCleanupRepoRoot)
	require.NoError(t, err)
	assert.Equal(t, githubRepo{Owner: "octo", Name: "widgets"}, repo)

	for _, fullName := range []string{"", "widgets", "/widgets", "octo/", "octo/widgets/extra"} {
		bad := &fakeRunner{outputs: map[string][]byte{
			"gh api " + githubRepoPath: []byte(fmt.Sprintf(`{"full_name":%q}`, fullName)),
		}}
		_, err := loadGitHubRepo(t.Context(), newGHClient(bad, nil), testCleanupRepoRoot)
		require.Error(t, err, fullName)
		assert.Contains(t, err.Error(), "invalid full_name", fullName)
	}
}

// closedPRPage renders a REST page of closed pull requests: the numbers in
// merged are merged, the rest were closed unmerged.
func closedPRPage(t *testing.T, numbers []int, merged map[int]bool) []byte {
	t.Helper()

	page := make([]map[string]any, 0, len(numbers))
	for _, number := range numbers {
		entry := map[string]any{"number": number, "merged_at": nil}
		if merged[number] {
			entry["merged_at"] = "2026-11-23T18:00:00Z"
		}
		page = append(page, entry)
	}
	data, err := json.Marshal(page)
	require.NoError(t, err)

	return data
}

// Validates: R-6.10.40
//
// Recent merged pull requests come from REST's closed list, newest first,
// skipping the ones closed unmerged and reading further pages only while too
// few were found.
func TestFetchRecentMergedPullRequestNumbersReadsPagesUntilEnoughAreMerged(t *testing.T) {
	t.Parallel()

	pagePath := func(page int) string {
		return fmt.Sprintf("gh api repos/octo/widgets/pulls?state=closed&sort=updated&direction=desc&per_page=%d&page=%d",
			mergedPRPageSize, page)
	}
	first := make([]int, 0, mergedPRPageSize)
	merged := map[int]bool{}
	for i := range mergedPRPageSize {
		number := 1000 - i
		first = append(first, number)
		merged[number] = i%50 == 0 // two merged on the first page, at its top and middle
	}
	runner := &fakeRunner{outputs: map[string][]byte{
		pagePath(1): closedPRPage(t, first, merged),
		pagePath(2): closedPRPage(t, []int{899, 898, 897}, map[int]bool{898: true, 897: true}),
	}}

	numbers, err := fetchRecentMergedPullRequestNumbers(t.Context(), newGHClient(runner, nil), testCleanupRepoRoot,
		githubRepo{Owner: "octo", Name: "widgets"}, 3)
	require.NoError(t, err)
	assert.Equal(t, []int{1000, 950, 898}, numbers)

	pagesRead := 0
	for _, command := range runner.outputCommands {
		if strings.Contains(strings.Join(command.args, " "), "/pulls?state=closed") {
			pagesRead++
		}
	}
	assert.Equal(t, 2, pagesRead, "the third page is never read once enough are found")
}
