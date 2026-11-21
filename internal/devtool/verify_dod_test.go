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
		Evidence:       []string{"TestContentFilter_ShouldObserveUnknownKindIncludesDirectoryCapablePaths", "go run ./cmd/devtool verify default"},
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
// step's gh commands and claim-ref git commands go to a claimRemote, every
// other command to a fakeRunner.
type nightlyStageRunner struct {
	*fakeRunner
	remote *claimRemote
}

// isNightlyStepCommand reports whether a command is one of the nightly
// step's: any gh command, and the git commands that read and write claims.
func isNightlyStepCommand(name string, args []string) bool {
	if name == githubCLI {
		return true
	}
	claimCommands := []string{"for-each-ref", "commit-tree", "push"}

	return name == nightlyTestGit && len(args) > 0 &&
		(slices.Contains(claimCommands, args[0]) || slices.Contains(args, nightlyClaimRefspec))
}

func (r *nightlyStageRunner) Run(ctx context.Context, cwd string, env []string, stdout, stderr io.Writer, name string, args ...string) error {
	if isNightlyStepCommand(name, args) {
		return r.remote.Run(ctx, cwd, env, stdout, stderr, name, args...)
	}
	return r.fakeRunner.Run(ctx, cwd, env, stdout, stderr, name, args...)
}

func (r *nightlyStageRunner) Output(ctx context.Context, cwd string, env []string, name string, args ...string) ([]byte, error) {
	if isNightlyStepCommand(name, args) {
		return r.remote.Output(ctx, cwd, env, name, args...)
	}
	return r.fakeRunner.Output(ctx, cwd, env, name, args...)
}

func (r *nightlyStageRunner) CombinedOutput(ctx context.Context, cwd string, env []string, name string, args ...string) ([]byte, error) {
	if isNightlyStepCommand(name, args) {
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

	require.Len(t, remote.pushes, 1, "the stage renews the branch's one claim")
	assert.True(t, strings.HasSuffix(remote.pushes[0], ":"+nightlyClaimRefPrefix+nightlyClaimRefName(nightlyTestKey)))
	assert.Contains(t, stdout.String(), "nightly: renewed "+nightlyTestOwner+"'s claim on "+nightlyTestKey+"\n")
	assert.Contains(t, stdout.String(), fmt.Sprintf("nightly: run %d on %s concluded failure", nightlyTestRunID, shortSHA(nightlyTestRunHead)))
	assert.Contains(t, stdout.String(), "  e2e: failed\n")
}

// Validates: R-6.10.16, R-6.10.23
//
// post-merge marks the nightly claims of --branch fixed and removes that
// branch, so a --branch that is not the pull request's head is refused
// before anything is merged.
func TestDODPostMergeRefusesABranchThatIsNotThePRsHead(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{outputs: map[string][]byte{
		"gh pr view 7 --json " + pullRequestStateFields: []byte(`{"state":"OPEN","headRefName":"fix/the-head"}`),
	}}
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:    writeWorkflowFixture(t, nightlyBoundWorkflow),
		DOD:         true,
		DODStage:    DODStagePostMerge,
		DODPR:       7,
		DODWorktree: "/worktrees/other",
		DODBranch:   "fix/another-branch",
		Stdout:      &bytes.Buffer{},
		Stderr:      &bytes.Buffer{},
	})
	require.Error(t, err)

	assert.Contains(t, err.Error(), "fix/another-branch")
	assert.Contains(t, err.Error(), "fix/the-head")
	assert.Empty(t, runner.combinedCommands, "nothing is merged")
	assert.Empty(t, runner.runCommands, "no claim is touched and no branch removed")
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
		Profile:      VerifyDefault,
		CoverageFile: coverageFile,
		DOD:          true,
		DODStage:     DODStagePrePR,
		Stdout:       &bytes.Buffer{},
		Stderr:       &bytes.Buffer{},
	})
	require.NoError(t, err)

	require.Len(t, runner.runCommands, verifyDefaultCommandCount+2, "the full tier, then the rebase check")
	assert.Equal(t, "-race", runner.runCommands[cmdUnitTests].args[1])
}

// Validates: R-6.10.16
func TestDODMergeWrapperTreatsMainWorktreeFailureAsServerSideMerge(t *testing.T) {
	t.Parallel()

	requireMergedPRAfterMergeError(t, "failed to run git: fatal: 'main' is already used by worktree\n")
}

// Validates: R-6.10.16
func TestDODMergeWrapperTreatsAlreadyMergedPRAsServerSideMerge(t *testing.T) {
	t.Parallel()

	requireMergedPRAfterMergeError(t, "Pull request #682 was already merged\n")
}

// Validates: R-6.10.16
func TestDODMergeWrapperTreatsAttachedBranchDeleteFailureAsServerSideMerge(t *testing.T) {
	t.Parallel()

	requireMergedPRAfterMergeError(t, "Pull request merged, but local branch fix/dod is checked out at a worktree\n")
}

func requireMergedPRAfterMergeError(t *testing.T, mergeOutput string) {
	t.Helper()

	runner := &fakeRunner{
		combinedOutputs: map[string][]byte{
			"gh pr merge 682 --squash --delete-branch": []byte(mergeOutput),
		},
		combinedErrByKey: map[string]error{
			"gh pr merge 682 --squash --delete-branch": errors.New("exit status 1"),
		},
		outputs: map[string][]byte{
			"gh pr view 682 --json " + pullRequestStateFields: []byte(`{"state":"MERGED","mergedAt":"2026-04-30T04:15:18Z","mergeCommit":{"oid":"merge-sha"}}`),
		},
	}

	state, err := mergePR(context.Background(), runner, newGHClient(runner, nil), testCleanupRepoRoot, 682)
	require.NoError(t, err)
	assert.Equal(t, "MERGED", state.State)
	assert.Equal(t, "merge-sha", state.MergeCommit.OID)
}

// Validates: R-6.10.16
func TestDODMergeWrapperFailsWhenServerSideMergeDidNotComplete(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{
		combinedOutputs: map[string][]byte{
			"gh pr merge 682 --squash --delete-branch": []byte("failed to run git: fatal: 'main' is already used by worktree\n"),
		},
		combinedErrByKey: map[string]error{
			"gh pr merge 682 --squash --delete-branch": errors.New("exit status 1"),
		},
		outputs: map[string][]byte{
			"gh pr view 682 --json " + pullRequestStateFields: []byte(`{"state":"OPEN"}`),
		},
	}

	_, err := mergePR(context.Background(), runner, newGHClient(runner, nil), testCleanupRepoRoot, 682)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server state is not merged")
}

// Validates: R-6.10.16
func TestDODBranchCleanupToleratesAlreadyDeletedBranches(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{
		combinedOutputs: map[string][]byte{
			"git branch -D feat/dod":            []byte("error: branch 'feat/dod' not found\n"),
			"git push origin --delete feat/dod": []byte("error: unable to delete 'feat/dod': remote ref does not exist\n"),
		},
		combinedErrByKey: map[string]error{
			"git branch -D feat/dod":            errors.New("exit status 1"),
			"git push origin --delete feat/dod": errors.New("exit status 1"),
		},
	}

	err := deleteDODIncrementBranches(context.Background(), runner, testCleanupRepoRoot, "feat/dod")

	require.NoError(t, err)
}

// Validates: R-6.10.16
func TestDODBranchCleanupRejectsAmbiguousRemoteDeleteFailure(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{
		combinedOutputs: map[string][]byte{
			"git push origin --delete feat/dod": []byte("error: unable to delete 'feat/dod': protected branch\n"),
		},
		combinedErrByKey: map[string]error{
			"git push origin --delete feat/dod": errors.New("exit status 1"),
		},
	}

	err := deleteDODIncrementBranches(context.Background(), runner, testCleanupRepoRoot, "feat/dod")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "protected branch")
}

func jsonUnmarshalString(data string, v any) error {
	decoder := json.NewDecoder(strings.NewReader(data))
	if err := decoder.Decode(v); err != nil {
		return fmt.Errorf("decode json fixture: %w", err)
	}
	return nil
}
