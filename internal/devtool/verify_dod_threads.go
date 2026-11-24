// The DoD stages' review threads: reading a pull request's and the recent
// merged ones' threads, applying the manifest's replies and resolves, and
// the gate that no thread is left unresolved.

package devtool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type dodReviewThreadApplySummary struct {
	CommentedAndResolved int
	AlreadyResolved      int
}

type dodReviewThreadGateSummary struct {
	CurrentPRThreads    int
	RecentMergedThreads int
}

const (
	// mergedPRPageSize is REST's largest page of pull requests.
	mergedPRPageSize = 100
	// mergedPRSearchPages bounds the pages read for recent merged pull
	// requests, should closed unmerged ones (dependency updates) crowd them.
	mergedPRSearchPages = 5
)

const singlePRReviewThreadsQuery = `
query($owner:String!, $repo:String!, $number:Int!, $after:String) {
  repository(owner:$owner, name:$repo) {
    pullRequest(number:$number) {
      number
      title
      url
      reviewThreads(first:100, after:$after) {
        pageInfo {
          hasNextPage
          endCursor
        }
        nodes {
          id
          isResolved
          isOutdated
          path
          line
          comments(first:20) {
            nodes {
              author { login }
              body
              url
              createdAt
            }
          }
        }
      }
    }
  }
}`

func fetchRecentMergedReviewThreads(
	ctx context.Context,
	gh ghClient,
	repoRoot string,
	repo githubRepo,
	recent int,
) ([]DODReviewThread, error) {
	prNumbers, err := fetchRecentMergedPullRequestNumbers(ctx, gh, repoRoot, repo, recent)
	if err != nil {
		return nil, err
	}

	threads := []DODReviewThread{}
	for _, prNumber := range prNumbers {
		prThreads, err := fetchPullRequestReviewThreads(ctx, gh, repoRoot, repo, prNumber)
		if err != nil {
			return nil, err
		}
		threads = append(threads, prThreads...)
	}
	return threads, nil
}

// fetchRecentMergedPullRequestNumbers lists the recent most recently updated
// merged pull requests, newest first. REST lists closed pull requests, merged
// or not, a page at a time, so pages are read until recent merged ones are
// found or mergedPRSearchPages pages have been read.
func fetchRecentMergedPullRequestNumbers(
	ctx context.Context,
	gh ghClient,
	repoRoot string,
	repo githubRepo,
	recent int,
) ([]int, error) {
	numbers := make([]int, 0, recent)
	for page := 1; page <= mergedPRSearchPages && len(numbers) < recent; page++ {
		path := fmt.Sprintf("repos/%s/%s/pulls?state=closed&sort=updated&direction=desc&per_page=%d&page=%d",
			repo.Owner, repo.Name, mergedPRPageSize, page)
		out, err := gh.output(ctx, repoRoot, "dod fetch recent merged PRs", "api", path)
		if err != nil {
			return nil, err
		}
		var closed []struct {
			Number   int     `json:"number"`
			MergedAt *string `json:"merged_at"`
		}
		if err := json.Unmarshal(out, &closed); err != nil {
			return nil, fmt.Errorf("decode recent merged PRs: %w", err)
		}
		for _, pr := range closed {
			if pr.MergedAt != nil && len(numbers) < recent {
				numbers = append(numbers, pr.Number)
			}
		}
		if len(closed) < mergedPRPageSize {
			break
		}
	}
	return numbers, nil
}

func fetchPullRequestReviewThreads(
	ctx context.Context,
	gh ghClient,
	repoRoot string,
	repo githubRepo,
	prNumber int,
) ([]DODReviewThread, error) {
	pr, err := fetchPullRequestReviewThreadPages(ctx, gh, repoRoot, repo, prNumber)
	if err != nil {
		// A Claude Code cloud session refuses GraphQL with 403 and serves the
		// threads over REST instead (verify_dod_threads_rest.go).
		if !isGitHubForbidden(err) {
			return nil, err
		}
		threads, restErr := fetchRestPullRequestReviewThreads(ctx, gh, repoRoot, prNumber)
		if restErr != nil {
			return nil, errRestThreadsUnavailable(err, restErr)
		}
		return threads, nil
	}
	return reviewThreadsFromPR(pr.Number, pr.Title, pr.URL, pr.ReviewThreads.Nodes), nil
}

func fetchPullRequestReviewThreadPages(
	ctx context.Context,
	gh ghClient,
	repoRoot string,
	repo githubRepo,
	prNumber int,
) (graphqlPullRequest, error) {
	var merged graphqlPullRequest
	cursor := ""
	for {
		page, err := fetchPullRequestReviewThreadPage(ctx, gh, repoRoot, repo, prNumber, cursor)
		if err != nil {
			return graphqlPullRequest{}, err
		}
		if merged.Number == 0 {
			merged.Number = page.Number
			merged.Title = page.Title
			merged.URL = page.URL
		}
		merged.ReviewThreads.Nodes = append(merged.ReviewThreads.Nodes, page.ReviewThreads.Nodes...)
		pageInfo := page.ReviewThreads.PageInfo
		if !pageInfo.HasNextPage {
			return merged, nil
		}
		if pageInfo.EndCursor == "" {
			return graphqlPullRequest{}, fmt.Errorf("dod fetch PR #%d review threads: missing next cursor", prNumber)
		}
		cursor = pageInfo.EndCursor
	}
}

func fetchPullRequestReviewThreadPage(
	ctx context.Context,
	gh ghClient,
	repoRoot string,
	repo githubRepo,
	prNumber int,
	cursor string,
) (graphqlPullRequest, error) {
	args := []string{
		"api",
		"graphql",
		"-f",
		"query=" + singlePRReviewThreadsQuery,
		"-f",
		"owner=" + repo.Owner,
		"-f",
		"repo=" + repo.Name,
		"-F",
		"number=" + strconv.Itoa(prNumber),
	}
	if cursor != "" {
		args = append(args, "-f", "after="+cursor)
	}
	out, err := gh.output(ctx, repoRoot, fmt.Sprintf("dod fetch PR #%d review threads", prNumber), args...)
	if err != nil {
		return graphqlPullRequest{}, err
	}

	var response singlePRThreadsResponse
	if err := json.Unmarshal(out, &response); err != nil {
		return graphqlPullRequest{}, fmt.Errorf("decode PR review threads: %w", err)
	}
	return response.Data.Repository.PullRequest, nil
}

type singlePRThreadsResponse struct {
	Data struct {
		Repository struct {
			PullRequest graphqlPullRequest
		}
	}
}

type graphqlPullRequest struct {
	Number        int
	Title         string
	URL           string
	ReviewThreads graphqlReviewThreadConnection
}

type graphqlReviewThreadConnection struct {
	PageInfo graphqlPageInfo
	Nodes    []graphqlReviewThread
}

type graphqlPageInfo struct {
	HasNextPage bool
	EndCursor   string
}

type graphqlReviewThread struct {
	ID         string
	IsResolved bool
	IsOutdated bool
	Path       string
	Line       *int
	Comments   struct {
		Nodes []graphqlReviewComment
	}
}

type graphqlReviewComment struct {
	Author struct {
		Login string
	}
	Body      string
	URL       string
	CreatedAt string
}

func reviewThreadsFromPR(prNumber int, prTitle string, prURL string, nodes []graphqlReviewThread) []DODReviewThread {
	threads := make([]DODReviewThread, 0, len(nodes))
	for i := range nodes {
		node := nodes[i]
		comments := make([]DODReviewComment, 0, len(node.Comments.Nodes))
		for j := range node.Comments.Nodes {
			comment := node.Comments.Nodes[j]
			comments = append(comments, DODReviewComment{
				Author:    comment.Author.Login,
				Body:      comment.Body,
				URL:       comment.URL,
				CreatedAt: comment.CreatedAt,
			})
		}
		threads = append(threads, DODReviewThread{
			ThreadID:      node.ID,
			SourcePR:      prNumber,
			SourcePRTitle: prTitle,
			SourcePRURL:   prURL,
			Path:          node.Path,
			Line:          node.Line,
			IsResolved:    node.IsResolved,
			IsOutdated:    node.IsOutdated,
			Comments:      comments,
		})
	}
	return threads
}

func applyManifestReviewThreads(
	ctx context.Context,
	runner commandRunner,
	gh ghClient,
	opts *VerifyOptions,
	stdout io.Writer,
) (dodReviewThreadApplySummary, error) {
	repo, err := loadGitHubRepo(ctx, gh, opts.RepoRoot)
	if err != nil {
		return dodReviewThreadApplySummary{}, err
	}
	currentPRThreads, err := fetchPullRequestReviewThreads(ctx, gh, opts.RepoRoot, repo, opts.DODPR)
	if err != nil {
		return dodReviewThreadApplySummary{}, err
	}
	manifestPath := dodManifestPath(opts)
	manifest, err := loadDODCommentManifest(manifestPath)
	if err != nil {
		return dodReviewThreadApplySummary{}, err
	}
	fetchedThreads := make([]DODReviewThread, 0, len(currentPRThreads)+len(manifest.Threads))
	fetchedThreads = append(fetchedThreads, currentPRThreads...)
	fetchedThreads = append(fetchedThreads, manifest.Threads...)
	manifest = mergeDODManifest(manifest, repo, dodRecentMerged(opts), fetchedThreads)
	writeErr := writeDODCommentManifest(manifestPath, manifest)
	if writeErr != nil {
		return dodReviewThreadApplySummary{}, writeErr
	}
	validateErr := validateDODCommentManifest(manifest)
	if validateErr != nil {
		return dodReviewThreadApplySummary{}, validateErr
	}
	commit, err := gitHeadShort(ctx, runner, opts.RepoRoot)
	if err != nil {
		return dodReviewThreadApplySummary{}, err
	}
	summary, changed, replyErr := replyToManifestThreads(ctx, gh, opts.RepoRoot, manifest.Threads, opts.DODPR, commit, stdout)
	if changed {
		// Record what is now resolved even when a later reply failed, so a
		// rerun never answers the same thread twice.
		if err := writeDODCommentManifest(manifestPath, manifest); err != nil {
			return dodReviewThreadApplySummary{}, errors.Join(replyErr, err)
		}
	}
	if replyErr != nil {
		return dodReviewThreadApplySummary{}, replyErr
	}
	return summary, nil
}

// replyToManifestThreads answers and resolves every classified thread that is
// still open on GitHub. The manifest's is_resolved is only a snapshot from when
// the manifest was written, and an ignored manifest outlives its increment, so
// each thread's live state is checked first; without that, every pre-merge
// run from the same checkout replied to the same long-resolved thread again,
// crediting whichever PR was merging. threads is updated in place, and changed
// reports whether any is_resolved flag moved so the caller can persist it.
func replyToManifestThreads(
	ctx context.Context,
	gh ghClient,
	repoRoot string,
	threads []DODReviewThread,
	prNumber int,
	commit string,
	stdout io.Writer,
) (dodReviewThreadApplySummary, bool, error) {
	summary := dodReviewThreadApplySummary{}
	changed := false
	for i := range threads {
		thread := &threads[i]
		if thread.IsResolved {
			summary.AlreadyResolved++
			continue
		}
		resolved, err := reviewThreadIsResolved(ctx, gh, repoRoot, thread.ThreadID)
		if err != nil {
			return summary, changed, err
		}
		if resolved {
			thread.IsResolved = true
			changed = true
			summary.AlreadyResolved++
			continue
		}
		body := renderDODThreadReply(thread, prNumber, commit)
		if _, err := postReviewThreadReply(ctx, gh, repoRoot, thread.ThreadID, body); err != nil {
			return summary, changed, err
		}
		if err := resolveReviewThread(ctx, gh, repoRoot, thread.ThreadID); err != nil {
			return summary, changed, err
		}
		thread.IsResolved = true
		changed = true
		summary.CommentedAndResolved++
		if err := writeStatus(stdout, formatDODThreadResolutionStatus(thread)); err != nil {
			return summary, changed, err
		}
	}
	return summary, changed, nil
}

// reviewThreadIsResolved reads one review thread's live resolution state.
func reviewThreadIsResolved(ctx context.Context, gh ghClient, repoRoot string, threadID string) (bool, error) {
	if prNumber, commentID, isRest := parseRestThreadID(threadID); isRest {
		return restThreadIsResolved(ctx, gh, repoRoot, prNumber, commentID)
	}
	out, err := gh.output(ctx, repoRoot, "dod read review thread "+threadID, buildThreadResolvedQueryArgs(threadID)...)
	if err != nil {
		return false, err
	}
	// Untagged like the other gh response structs: encoding/json matches
	// GitHub's camelCase keys case-insensitively.
	var response struct {
		Data struct {
			Node *struct {
				IsResolved bool
			}
		}
	}
	if err := json.Unmarshal(out, &response); err != nil {
		return false, fmt.Errorf("decode review thread %s state: %w", threadID, err)
	}
	if response.Data.Node == nil {
		return false, fmt.Errorf("dod read review thread %s: thread not found", threadID)
	}
	return response.Data.Node.IsResolved, nil
}

func formatDODThreadResolutionStatus(thread *DODReviewThread) string {
	return fmt.Sprintf(
		"dod pre-merge: commented and resolved %s classification=%s evidence=%d\n",
		thread.ThreadID,
		thread.Classification,
		len(nonEmptyStrings(thread.Evidence)),
	)
}

func renderDODThreadReply(thread *DODReviewThread, fixPR int, commit string) string {
	var body bytes.Buffer
	switch thread.Classification {
	case DODCommentFixed:
		renderDODFixedThreadReply(&body, thread, fixPR, commit)
	case DODCommentAlreadyFixed:
		renderDODFixedThreadReply(&body, thread, fixPR, commit)
	case DODCommentNonActionable:
		fmt.Fprintf(&body, "Resolved as non-actionable during #%d", fixPR)
		if commit != "" {
			fmt.Fprintf(&body, " (%s)", commit)
		}
		fmt.Fprintf(&body, ".\n\nReason: %s", strings.TrimSpace(thread.Reason))
	default:
		fmt.Fprintf(&body, "Unable to render unclassified thread %s.", thread.ThreadID)
	}
	return body.String()
}

func renderDODFixedThreadReply(body *bytes.Buffer, thread *DODReviewThread, fixPR int, commit string) {
	fmt.Fprintf(body, "Addressed in #%d", fixPR)
	if commit != "" {
		fmt.Fprintf(body, " (%s)", commit)
	}
	fmt.Fprintf(
		body,
		".\n\nWhat changed: %s\n\nHow fixed: %s",
		strings.TrimSpace(thread.WhatChanged),
		strings.TrimSpace(thread.HowFixed),
	)
	evidence := nonEmptyStrings(thread.Evidence)
	if len(evidence) > 0 {
		fmt.Fprintf(body, "\n\nEvidence:")
		for _, item := range evidence {
			fmt.Fprintf(body, "\n- %s", strings.TrimSpace(item))
		}
	}
}

// postReviewThreadReply posts one reply, once: GitHub can answer a 5xx after
// it posted, and a repeat would post the reply twice.
func postReviewThreadReply(ctx context.Context, gh ghClient, repoRoot string, threadID string, body string) (string, error) {
	if prNumber, commentID, isRest := parseRestThreadID(threadID); isRest {
		return postRestThreadReply(ctx, gh, repoRoot, prNumber, commentID, body)
	}
	out, err := gh.outputOnce(ctx, repoRoot, "dod reply to review thread "+threadID, buildAddThreadReplyArgs(threadID, body)...)
	if err != nil {
		return "", err
	}
	var response struct {
		Data struct {
			AddPullRequestReviewThreadReply struct {
				Comment struct {
					URL string
				}
			}
		}
	}
	if err := json.Unmarshal(out, &response); err != nil {
		return "", fmt.Errorf("decode review thread reply response: %w", err)
	}
	return response.Data.AddPullRequestReviewThreadReply.Comment.URL, nil
}

// resolveReviewThread resolves one thread. Resolving a resolved thread
// changes nothing, so a failed attempt is repeated.
func resolveReviewThread(ctx context.Context, gh ghClient, repoRoot string, threadID string) error {
	if prNumber, commentID, isRest := parseRestThreadID(threadID); isRest {
		return resolveRestThread(ctx, gh, repoRoot, prNumber, commentID)
	}
	if _, err := gh.output(ctx, repoRoot, "dod resolve review thread "+threadID, buildResolveThreadArgs(threadID)...); err != nil {
		return err
	}
	return nil
}

func buildAddThreadReplyArgs(threadID string, body string) []string {
	return []string{
		"api",
		"graphql",
		"-f",
		"query=mutation($threadId:ID!, $body:String!){addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$threadId, body:$body}){comment{url}}}", //nolint:lll // GraphQL mutation string must stay exact for gh api.
		"-F",
		"threadId=" + threadID,
		"-F",
		"body=" + body,
	}
}

func buildThreadResolvedQueryArgs(threadID string) []string {
	return []string{
		"api",
		"graphql",
		"-f",
		"query=query($threadId:ID!){node(id:$threadId){... on PullRequestReviewThread{isResolved}}}",
		"-F",
		"threadId=" + threadID,
	}
}

func buildResolveThreadArgs(threadID string) []string {
	return []string{
		"api",
		"graphql",
		"-f",
		"query=mutation($threadId:ID!){resolveReviewThread(input:{threadId:$threadId}){thread{id isResolved}}}",
		"-F",
		"threadId=" + threadID,
	}
}

func requireNoUnresolvedPRThreads(
	ctx context.Context,
	gh ghClient,
	repoRoot string,
	prNumber int,
	recentMerged int,
) (dodReviewThreadGateSummary, error) {
	repo, err := loadGitHubRepo(ctx, gh, repoRoot)
	if err != nil {
		return dodReviewThreadGateSummary{}, err
	}
	threads, err := fetchPullRequestReviewThreads(ctx, gh, repoRoot, repo, prNumber)
	if err != nil {
		return dodReviewThreadGateSummary{}, err
	}
	summary := dodReviewThreadGateSummary{CurrentPRThreads: len(threads)}
	unresolved := unresolvedThreadIDs(threads)
	if len(unresolved) > 0 {
		return dodReviewThreadGateSummary{}, fmt.Errorf(
			"dod pre-merge unresolved review threads remain on PR #%d: %s",
			prNumber,
			strings.Join(unresolved, ", "),
		)
	}
	recent, err := fetchRecentMergedReviewThreads(ctx, gh, repoRoot, repo, recentMerged)
	if err != nil {
		return dodReviewThreadGateSummary{}, err
	}
	summary.RecentMergedThreads = len(recent)
	if unresolved = unresolvedThreadIDs(recent); len(unresolved) > 0 {
		return dodReviewThreadGateSummary{}, fmt.Errorf(
			"dod pre-merge unresolved recent merged-PR review threads remain: %s",
			strings.Join(unresolved, ", "),
		)
	}
	return summary, nil
}

func unresolvedThreadIDs(threads []DODReviewThread) []string {
	unresolved := []string{}
	for i := range threads {
		thread := threads[i]
		if !thread.IsResolved {
			unresolved = append(unresolved, thread.ThreadID)
		}
	}
	return unresolved
}
