package devtool

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// graphqlRefusedByCloudSession is how a Claude Code cloud session's proxy
// answers any GraphQL request.
const graphqlRefusedByCloudSession = "HTTP 403: GitHub GraphQL is not available from Claude Code sessions; " +
	"use the REST API (https://api.github.com/graphql)"

// restThreadsRunner answers GraphQL with refusal, and REST from outputs keyed
// by the gh arguments; it records every call.
type restThreadsRunner struct {
	fakeRunner
	graphqlErr error
	rest       map[string]string
	restErr    map[string]error
}

func newRestThreadsRunner() *restThreadsRunner {
	r := &restThreadsRunner{
		graphqlErr: ghFailure(graphqlRefusedByCloudSession),
		rest: map[string]string{
			"api " + githubRepoPath + "/pulls/683": `{"title":"DoD automation","html_url":"https://github.test/pull/683",` +
				`"head":{"ref":"feat/x","sha":"abc"}}`,
			"api " + githubRepoPath + "/pulls/683/ccr/review_threads": `[` +
				`{"resolved":false,"outdated":true,"path":"a.go","line":7,"comment_ids":[11,12]},` +
				`{"resolved":true,"outdated":false,"path":"b.go","line":null,"comment_ids":[21]}]`,
			"api " + githubRepoPath + "/pulls/comments/11": `{"user":{"login":"reviewer"},"body":"Please fix",` +
				`"html_url":"https://github.test/c/11","created_at":"2026-11-01T00:00:00Z"}`,
			"api " + githubRepoPath + "/pulls/comments/12": `{"user":{"login":"author"},"body":"Will do",` +
				`"html_url":"https://github.test/c/12","created_at":"2026-11-02T00:00:00Z"}`,
			"api " + githubRepoPath + "/pulls/comments/21": `{"user":{"login":"bot"},"body":"Quota notice",` +
				`"html_url":"https://github.test/c/21","created_at":"2026-11-03T00:00:00Z"}`,
		},
		restErr: map[string]error{},
	}
	r.outputFunc = func(_ string, _ []string, _ string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[0] == "api" && args[1] == "graphql" {
			return nil, r.graphqlErr
		}
		key := strings.Join(args, " ")
		for prefix, err := range r.restErr {
			if strings.HasPrefix(key, prefix) {
				return nil, err
			}
		}
		if strings.Contains(key, "/replies -f body=") {
			return []byte(`{"html_url":"https://github.test/c/13"}`), nil
		}
		if strings.HasSuffix(key, "/resolve") {
			return []byte(`{}`), nil
		}
		out, ok := r.rest[key]
		if !ok {
			return nil, errors.New("unscripted gh call: " + key)
		}
		return []byte(out), nil
	}

	return r
}

func (r *restThreadsRunner) calls() []string {
	keys := make([]string, 0, len(r.outputCommands))
	for _, command := range r.outputCommands {
		keys = append(keys, strings.Join(command.args, " "))
	}

	return keys
}

// Validates: R-6.10.40
//
// Where GraphQL is refused with 403, as in a Claude Code cloud session, a pull
// request's review threads are read over REST with the same facts: state,
// place, comments, and the pull request they came from.
func TestFetchPullRequestReviewThreadsReadsThemOverRESTWhenGraphQLIsRefused(t *testing.T) {
	t.Parallel()

	runner := newRestThreadsRunner()
	threads, err := fetchPullRequestReviewThreads(context.Background(), newGHClient(runner, nil), testCleanupRepoRoot,
		githubRepo{Owner: "octo", Name: "widgets"}, 683)
	require.NoError(t, err)
	require.Len(t, threads, 2)

	line := 7
	assert.Equal(t, DODReviewThread{
		ThreadID: "rest:683/11", SourcePR: 683, SourcePRTitle: "DoD automation", SourcePRURL: "https://github.test/pull/683",
		Path: "a.go", Line: &line, IsResolved: false, IsOutdated: true,
		Comments: []DODReviewComment{
			{Author: "reviewer", Body: "Please fix", URL: "https://github.test/c/11", CreatedAt: "2026-11-01T00:00:00Z"},
			{Author: "author", Body: "Will do", URL: "https://github.test/c/12", CreatedAt: "2026-11-02T00:00:00Z"},
		},
	}, threads[0])
	assert.Equal(t, "rest:683/21", threads[1].ThreadID)
	assert.True(t, threads[1].IsResolved)
	assert.Nil(t, threads[1].Line)
}

// Validates: R-6.10.40
//
// A GraphQL failure other than 403 is the answer: it is not papered over by
// another route.
func TestFetchPullRequestReviewThreadsKeepsAGraphQLFailureThatIsNotARefusal(t *testing.T) {
	t.Parallel()

	runner := newRestThreadsRunner()
	runner.graphqlErr = ghFailure("HTTP 404: Not Found (https://api.github.com/graphql)")
	_, err := fetchPullRequestReviewThreads(context.Background(), newGHClient(runner, nil), testCleanupRepoRoot,
		githubRepo{Owner: "octo", Name: "widgets"}, 683)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 404")
	for _, call := range runner.calls() {
		assert.NotContains(t, call, "/ccr/", "no REST read follows a GraphQL failure that is not a refusal")
	}
}

// Validates: R-6.10.40
//
// When the REST stand-in fails too, both failures are reported.
func TestFetchPullRequestReviewThreadsReportsBothFailuresWhenRESTFailsToo(t *testing.T) {
	t.Parallel()

	runner := newRestThreadsRunner()
	runner.restErr["api "+githubRepoPath+"/pulls/683/ccr/"] = ghFailure("HTTP 404: Not Found (https://api.github.com/repos/o/r/pulls/683/ccr/review_threads)")
	_, err := fetchPullRequestReviewThreads(context.Background(), newGHClient(runner, nil), testCleanupRepoRoot,
		githubRepo{Owner: "octo", Name: "widgets"}, 683)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GraphQL is not available")
	assert.Contains(t, err.Error(), "read review threads over REST instead")
	assert.Contains(t, err.Error(), "HTTP 404")
}

// Validates: R-6.10.40
//
// A thread read over REST is answered, checked and resolved over REST: its ID
// names the route, so no call goes to GraphQL.
func TestRestThreadIsRepliedToCheckedAndResolvedOverREST(t *testing.T) {
	t.Parallel()

	runner := newRestThreadsRunner()
	gh := newGHClient(runner, nil)
	ctx := context.Background()

	resolved, err := reviewThreadIsResolved(ctx, gh, testCleanupRepoRoot, "rest:683/11")
	require.NoError(t, err)
	assert.False(t, resolved)
	resolved, err = reviewThreadIsResolved(ctx, gh, testCleanupRepoRoot, "rest:683/21")
	require.NoError(t, err)
	assert.True(t, resolved)
	_, err = reviewThreadIsResolved(ctx, gh, testCleanupRepoRoot, "rest:683/99")
	require.ErrorContains(t, err, "thread not found")

	url, err := postReviewThreadReply(ctx, gh, testCleanupRepoRoot, "rest:683/11", "Fixed in #990.")
	require.NoError(t, err)
	assert.Equal(t, "https://github.test/c/13", url)
	require.NoError(t, resolveReviewThread(ctx, gh, testCleanupRepoRoot, "rest:683/11"))

	calls := runner.calls()
	assert.Contains(t, calls, "api -X POST "+githubRepoPath+"/pulls/683/comments/11/replies -f body=Fixed in #990.")
	assert.Contains(t, calls, "api -X POST "+githubRepoPath+"/pulls/683/ccr/comments/11/resolve")
	for _, call := range calls {
		assert.NotContains(t, call, "graphql")
	}
}

// Validates: R-6.10.40
func TestParseRestThreadIDAcceptsOnlyItsOwnForm(t *testing.T) {
	t.Parallel()

	prNumber, commentID, ok := parseRestThreadID(restThreadID(683, 11))
	require.True(t, ok)
	assert.Equal(t, 683, prNumber)
	assert.Equal(t, int64(11), commentID)
	for _, id := range []string{"PRRT_kwDO123", "rest:", "rest:683", "rest:683/", "rest:/11", "rest:x/11", "rest:683/y", "rest:0/11", "rest:683/-1"} {
		_, _, ok := parseRestThreadID(id)
		assert.False(t, ok, id)
	}
}
