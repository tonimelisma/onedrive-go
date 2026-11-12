package devtool

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"time"
)

const (
	// githubAttempts bounds how often one GitHub request is made: the first
	// try and three repeats.
	githubAttempts = 4
	// githubFirstBackoff is the wait before the first repeat; each later wait
	// doubles it, so a request that keeps failing gives up after 35 seconds
	// of waiting. GitHub's 5xx bursts have lasted several requests in a row.
	githubFirstBackoff = 5 * time.Second
)

var (
	// githubServerErrorPattern finds the status of a 5xx answer in gh's
	// error text: `HTTP 502: Bad Gateway (url)` from most commands, `Bad
	// Gateway (HTTP 502)` from `gh api`.
	githubServerErrorPattern = regexp.MustCompile(`HTTP (5\d{2})\b`)

	// githubDroppedConnectionPattern finds the ways Go's HTTP client reports
	// a connection that ended before GitHub answered.
	githubDroppedConnectionPattern = regexp.MustCompile(
		`(?m)connection reset by peer|broken pipe|unexpected EOF|server sent GOAWAY|: EOF$`)

	// githubUnreachablePattern finds the ways Go's HTTP client reports a
	// request that never reached GitHub: the runner's network or DNS failed.
	githubUnreachablePattern = regexp.MustCompile(
		`(?i)i/o timeout|TLS handshake timeout|connection refused|no such host|` +
			`temporary failure in name resolution|network is unreachable`)
)

// ghClient runs the gh requests of the DoD stages and the nightly check. It
// owns one policy: a request GitHub answered with a 5xx, whose connection
// dropped, or that never reached GitHub says nothing about the request and is
// repeated with backoff, at most githubAttempts times in all; any other
// failure is final at once. A rate limit is final too, although it passes:
// GitHub answers it about the rate of requests, a secondary limit asks for
// minutes of quiet, and repeating within seconds only extends it, so the
// stage fails naming it and is run again later. A request that must not
// happen twice goes through outputOnce, and one GitHub refuses rather than
// doubles through outputUnlessDone.
type ghClient struct {
	runner commandRunner
	// sleep waits out a backoff; tests pass a clock that returns at once.
	sleep func(context.Context, time.Duration) error
}

// newGHClient returns the gh boundary over runner. A nil sleep waits on the
// real clock.
func newGHClient(runner commandRunner, sleep func(context.Context, time.Duration) error) ghClient {
	if sleep == nil {
		sleep = sleepContext
	}

	return ghClient{runner: runner, sleep: sleep}
}

// output runs one gh request that is safe to repeat, a read or a request
// whose repeat changes nothing, and returns its stdout. call names the
// request in every error.
func (c ghClient) output(ctx context.Context, repoRoot string, call string, args ...string) ([]byte, error) {
	out, _, err := c.request(ctx, repoRoot, call, nil, args...)

	return out, err
}

// outputUnlessDone runs one gh request that changes something on GitHub and
// that GitHub can carry out while still answering it with a 5xx. Its repeat
// is then refused because the change is in place; when a repeat after a
// transient failure fails in the way alreadyDone recognizes, the request
// counts as made, and done reports it. The same refusal to a first attempt is
// an ordinary failure: nothing this call sent made the change.
func (c ghClient) outputUnlessDone(
	ctx context.Context,
	repoRoot string,
	call string,
	alreadyDone func(error) bool,
	args ...string,
) (bool, error) {
	_, done, err := c.request(ctx, repoRoot, call, alreadyDone, args...)

	return done, err
}

// request makes one gh request under the retry policy. done is true when a
// repeat was refused as alreadyDone recognizes.
func (c ghClient) request(
	ctx context.Context,
	repoRoot string,
	call string,
	alreadyDone func(error) bool,
	args ...string,
) ([]byte, bool, error) {
	wait := githubFirstBackoff
	for attempt := 1; ; attempt++ {
		out, err := c.runner.Output(ctx, repoRoot, os.Environ(), githubCLI, args...)
		if err == nil {
			return out, false, nil
		}
		cause, transient := transientGitHubFailure(err)
		if !transient {
			if attempt > 1 && alreadyDone != nil && alreadyDone(err) {
				return nil, true, nil
			}

			return nil, false, fmt.Errorf("%s: %w", call, err)
		}
		if attempt == githubAttempts {
			return nil, false, fmt.Errorf("%s: GitHub %s on all %d attempts: %w", call, cause, githubAttempts, err)
		}
		if sleepErr := c.sleep(ctx, wait); sleepErr != nil {
			return nil, false, fmt.Errorf("%s: wait to repeat after GitHub %s: %w", call, cause, sleepErr)
		}
		wait *= 2
	}
}

// outputOnce runs one gh request that must not be repeated, such as posting
// a review reply: a 5xx can come after GitHub acted, and a repeat would act
// twice.
func (c ghClient) outputOnce(ctx context.Context, repoRoot string, call string, args ...string) ([]byte, error) {
	out, err := c.runner.Output(ctx, repoRoot, os.Environ(), githubCLI, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", call, err)
	}

	return out, nil
}

// transientGitHubFailure reports whether a failed gh request failed in a way
// that says nothing about the request, and how, for the message.
func transientGitHubFailure(err error) (string, bool) {
	text := err.Error()
	if m := githubServerErrorPattern.FindStringSubmatch(text); m != nil {
		return "answered HTTP " + m[1], true
	}
	if githubDroppedConnectionPattern.MatchString(text) {
		return "dropped the connection", true
	}
	if githubUnreachablePattern.MatchString(text) {
		return "could not be reached", true
	}

	return "", false
}
