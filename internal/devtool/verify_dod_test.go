package devtool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

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
		runner,
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
			HowFixed:       "Preserved the manual resolution notes.",
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
	assert.Equal(t, "Preserved the manual resolution notes.", thread.HowFixed)
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

	name, args := buildAddThreadReplyArgs("thread-1", "body text")
	assert.Equal(t, githubCLI, name)
	assert.Equal(t, []string{
		"api",
		"graphql",
		"-f",
		"query=mutation($threadId:ID!, $body:String!){addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$threadId, body:$body}){comment{url}}}",
		"-F",
		"threadId=thread-1",
		"-F",
		"body=body text",
	}, args)

	name, args = buildResolveThreadArgs("thread-1")
	assert.Equal(t, githubCLI, name)
	assert.Equal(t, []string{
		"api",
		"graphql",
		"-f",
		"query=mutation($threadId:ID!){resolveReviewThread(input:{threadId:$threadId}){thread{id isResolved}}}",
		"-F",
		"threadId=thread-1",
	}, args)
}

// Validates: R-6.10.16
func TestDODPRCIClassification(t *testing.T) {
	t.Parallel()

	status, reason := classifyPRChecks([]prCheckRun{
		{Name: "verify", Status: "COMPLETED", Conclusion: "SUCCESS"},
		{Name: "integration", Status: "COMPLETED", Conclusion: "SUCCESS"},
		{Name: "e2e", Status: "COMPLETED", Conclusion: "SUCCESS"},
		{Name: "stress", Status: "COMPLETED", Conclusion: "SKIPPED"},
	})
	assert.Equal(t, prCheckStatePass, status)
	assert.Contains(t, reason, "passed")

	status, reason = classifyPRChecks([]prCheckRun{
		{Name: "verify", Status: "COMPLETED", Conclusion: "SUCCESS"},
		{Name: "integration", Status: "COMPLETED", Conclusion: "SKIPPED"},
		{Name: "e2e", Status: "COMPLETED", Conclusion: "SUCCESS"},
	})
	assert.Equal(t, prCheckStateFail, status)
	assert.Contains(t, reason, "integration concluded skipped")
}

// Validates: R-6.10.16
func TestDODPostMergeCIClassificationAcceptsSquashSkip(t *testing.T) {
	t.Parallel()

	status, reason := classifyPostMergeRuns([]githubRun{{
		Name:       "CI",
		Event:      "push",
		Status:     checkStatusCompleted,
		Conclusion: checkConclusionSkipped,
		HeadSHA:    "merge-sha",
	}}, "fix(sync): address stale PR review threads (#682)", "merge-sha")

	assert.Equal(t, prCheckStatePass, status)
	assert.Contains(t, reason, "skipped by squash-merge push rule")

	status, reason = classifyPostMergeRuns([]githubRun{{
		Name:       "CI",
		Event:      "push",
		Status:     checkStatusCompleted,
		Conclusion: checkConclusionSkipped,
		HeadSHA:    "merge-sha",
	}}, "manual push", "merge-sha")
	assert.Equal(t, prCheckStateFail, status)
	assert.Contains(t, reason, "without squash-merge subject")
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
			"gh pr view 682 --json state,mergedAt,mergeCommit,statusCheckRollup,url": []byte(`{"state":"MERGED","mergedAt":"2026-04-30T04:15:18Z","mergeCommit":{"oid":"merge-sha"}}`),
		},
	}

	state, err := mergePR(context.Background(), runner, testCleanupRepoRoot, 682)
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
			"gh pr view 682 --json state,mergedAt,mergeCommit,statusCheckRollup,url": []byte(`{"state":"OPEN"}`),
		},
	}

	_, err := mergePR(context.Background(), runner, testCleanupRepoRoot, 682)
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
