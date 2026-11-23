package devtool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Review threads over REST, for a Claude Code cloud session.
//
// GitHub keeps a review thread's resolution in GraphQL alone, and a cloud
// session's GitHub proxy refuses GraphQL (HTTP 403). The proxy serves the same
// facts on REST routes of its own under /pulls/{n}/ccr/: a pull request's
// threads, each with its resolved and outdated state, path, line and comment
// IDs, and resolving a thread by one of its comments. The comments themselves,
// and replies to them, are GitHub's ordinary REST.
//
// The DoD stages read threads over GraphQL, and read them here only when
// GraphQL answered 403; the thread IDs they then record carry this route's
// prefix, so a reply or a resolve later goes back the way the thread came,
// with no second guess.

const (
	// restThreadIDPrefix starts the ID of a thread read over REST:
	// rest:<pull request>/<first comment ID>.
	restThreadIDPrefix = "rest:"

	// reviewThreadCommentLimit bounds the comments read per thread, as the
	// GraphQL query reads the first 20.
	reviewThreadCommentLimit = 20

	httpForbidden = "403"
	httpNotFound  = "404"
)

// restReviewThread is one thread as the proxy's review_threads route lists it.
type restReviewThread struct {
	Resolved   bool    `json:"resolved"`
	Outdated   bool    `json:"outdated"`
	Path       string  `json:"path"`
	Line       *int    `json:"line"`
	CommentIDs []int64 `json:"comment_ids"`
}

// restReviewComment is a pull request review comment as GitHub's REST
// returns it.
type restReviewComment struct {
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	Body      string `json:"body"`
	HTMLURL   string `json:"html_url"`
	CreatedAt string `json:"created_at"`
}

// isGitHubForbidden reports whether a gh request failed with HTTP 403.
func isGitHubForbidden(err error) bool {
	return githubHTTPStatus(err) == httpForbidden
}

// githubHTTPStatus is the HTTP status a failed gh request names, or "" when
// it names none.
func githubHTTPStatus(err error) string {
	const statusGroup = 1
	m := githubStatusPattern.FindStringSubmatch(err.Error())
	if len(m) <= statusGroup {
		return ""
	}

	return m[statusGroup]
}

func restThreadID(prNumber int, commentID int64) string {
	return restThreadIDPrefix + strconv.Itoa(prNumber) + "/" + strconv.FormatInt(commentID, 10)
}

// parseRestThreadID splits a REST thread ID; ok is false for any other ID.
func parseRestThreadID(threadID string) (int, int64, bool) {
	rest, isRest := strings.CutPrefix(threadID, restThreadIDPrefix)
	if !isRest {
		return 0, 0, false
	}
	prPart, commentPart, found := strings.Cut(rest, "/")
	prNumber, prErr := strconv.Atoi(prPart)
	commentID, commentErr := strconv.ParseInt(commentPart, 10, 64)
	if !found || prErr != nil || commentErr != nil || prNumber <= 0 || commentID <= 0 {
		return 0, 0, false
	}

	return prNumber, commentID, true
}

func listRestReviewThreads(ctx context.Context, gh ghClient, repoRoot string, prNumber int) ([]restReviewThread, error) {
	number := strconv.Itoa(prNumber)
	out, err := gh.output(ctx, repoRoot, "dod review threads of PR #"+number+" over REST",
		"api", githubRepoPath+"/pulls/"+number+"/ccr/review_threads")
	if err != nil {
		return nil, err
	}
	var threads []restReviewThread
	if err := json.Unmarshal(out, &threads); err != nil {
		return nil, fmt.Errorf("decode review threads of PR #%d: %w", prNumber, err)
	}

	return threads, nil
}

// fetchRestPullRequestReviewThreads reads a pull request's review threads
// over REST, the way the GraphQL query reads them.
func fetchRestPullRequestReviewThreads(ctx context.Context, gh ghClient, repoRoot string, prNumber int) ([]DODReviewThread, error) {
	pr, err := loadPullRequest(ctx, gh, repoRoot, prNumber)
	if err != nil {
		return nil, err
	}
	listed, err := listRestReviewThreads(ctx, gh, repoRoot, prNumber)
	if err != nil {
		return nil, err
	}

	threads := make([]DODReviewThread, 0, len(listed))
	for i := range listed {
		thread := listed[i]
		if len(thread.CommentIDs) == 0 {
			return nil, fmt.Errorf("dod review threads of PR #%d: a thread lists no comment", prNumber)
		}
		comments := make([]DODReviewComment, 0, min(len(thread.CommentIDs), reviewThreadCommentLimit))
		for _, commentID := range thread.CommentIDs[:min(len(thread.CommentIDs), reviewThreadCommentLimit)] {
			comment, err := loadRestReviewComment(ctx, gh, repoRoot, commentID)
			if err != nil {
				return nil, err
			}
			comments = append(comments, DODReviewComment{
				Author:    comment.User.Login,
				Body:      comment.Body,
				URL:       comment.HTMLURL,
				CreatedAt: comment.CreatedAt,
			})
		}
		threads = append(threads, DODReviewThread{
			ThreadID:      restThreadID(prNumber, thread.CommentIDs[0]),
			SourcePR:      prNumber,
			SourcePRTitle: pr.Title,
			SourcePRURL:   pr.HTMLURL,
			Path:          thread.Path,
			Line:          thread.Line,
			IsResolved:    thread.Resolved,
			IsOutdated:    thread.Outdated,
			Comments:      comments,
		})
	}

	return threads, nil
}

func loadRestReviewComment(ctx context.Context, gh ghClient, repoRoot string, commentID int64) (restReviewComment, error) {
	id := strconv.FormatInt(commentID, 10)
	out, err := gh.output(ctx, repoRoot, "dod review comment "+id, "api", githubRepoPath+"/pulls/comments/"+id)
	if err != nil {
		return restReviewComment{}, err
	}
	var comment restReviewComment
	if err := json.Unmarshal(out, &comment); err != nil {
		return restReviewComment{}, fmt.Errorf("decode review comment %s: %w", id, err)
	}

	return comment, nil
}

// restThreadIsResolved reads a REST thread's live resolution state: the
// thread of its pull request whose comments include its first.
func restThreadIsResolved(ctx context.Context, gh ghClient, repoRoot string, prNumber int, commentID int64) (bool, error) {
	threads, err := listRestReviewThreads(ctx, gh, repoRoot, prNumber)
	if err != nil {
		return false, err
	}
	for i := range threads {
		for _, id := range threads[i].CommentIDs {
			if id == commentID {
				return threads[i].Resolved, nil
			}
		}
	}

	return false, fmt.Errorf("dod read review thread %s: thread not found", restThreadID(prNumber, commentID))
}

// postRestThreadReply answers a thread by replying to its first comment, and
// returns the reply's URL. A reply is sent once: a repeat would post twice.
func postRestThreadReply(ctx context.Context, gh ghClient, repoRoot string, prNumber int, commentID int64, body string) (string, error) {
	threadID := restThreadID(prNumber, commentID)
	out, err := gh.outputOnce(ctx, repoRoot, "dod reply to review thread "+threadID,
		"api", "-X", "POST",
		githubRepoPath+"/pulls/"+strconv.Itoa(prNumber)+"/comments/"+strconv.FormatInt(commentID, 10)+"/replies",
		"-f", "body="+body)
	if err != nil {
		return "", err
	}
	var reply restReviewComment
	if err := json.Unmarshal(out, &reply); err != nil {
		return "", fmt.Errorf("decode review thread reply response: %w", err)
	}

	return reply.HTMLURL, nil
}

// resolveRestThread resolves a thread by its first comment. Resolving a
// resolved thread changes nothing, so a failed attempt is repeated.
func resolveRestThread(ctx context.Context, gh ghClient, repoRoot string, prNumber int, commentID int64) error {
	_, err := gh.output(ctx, repoRoot, "dod resolve review thread "+restThreadID(prNumber, commentID),
		"api", "-X", "POST",
		githubRepoPath+"/pulls/"+strconv.Itoa(prNumber)+"/ccr/comments/"+strconv.FormatInt(commentID, 10)+"/resolve")

	return err
}

// errRestThreadsUnavailable joins a refused GraphQL read with the failure of
// its REST stand-in, so neither cause is lost.
func errRestThreadsUnavailable(graphqlErr, restErr error) error {
	return errors.Join(graphqlErr, fmt.Errorf("read review threads over REST instead: %w", restErr))
}
